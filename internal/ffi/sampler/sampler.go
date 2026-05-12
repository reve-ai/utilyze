package sampler

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

var (
	ErrSamplerNotInitialized           = errors.New("sampler not initialized")
	ErrSamplerCouldNotInitialize       = errors.New("sampler could not initialize")
	ErrSamplerNoMetrics                = errors.New("sampler has no metrics")
	ErrSamplerPollEmpty                = errors.New("sampler poll returned no data")
	ErrSamplerInsufficientCapabilities = errors.New("sampler requires CAP_SYS_ADMIN")
)

var initState struct {
	mu          sync.Mutex
	initialized map[int]struct{}
}

type Sampler struct {
	handle               samplerHandle
	mu                   sync.Mutex
	metricCount          int32
	metricIndexByName    map[string]int32
	polledValues         []float64
	initializedDeviceIDs []int

	// NOTE: not used, out-params for poll
	pmSamples  int32
	groupId    int32
	groupCount int32
}

type Snapshot struct {
	DeviceID      int
	ComputeSOLPct *float64
	MemorySOLPct  *float64
	SMActivePct   *float64
	// Pipes holds the per-pipe pct_of_peak_sustained_elapsed values keyed by
	// short pipe name (see Pipes). Compute/Memory SOL above are the max of
	// the compute/memory subsets respectively; this map exposes the breakdown
	// so consumers can see which pipe is the bottleneck.
	Pipes     map[string]float64
	Timestamp time.Time
}

func Init(deviceIds []int, metrics []string, interval time.Duration) (*Sampler, error) {
	if sha256sum, err := load(); err != nil {
		return nil, fmt.Errorf("failed to load native module (sha256: %s): %w", sha256sum, err)
	}

	ids := make([]int32, len(deviceIds))
	for i, id := range deviceIds {
		ids[i] = int32(id)
	}

	metricsCsv := strings.Join(metrics, ",")

	initState.mu.Lock()
	defer initState.mu.Unlock()
	if initState.initialized == nil {
		initState.initialized = make(map[int]struct{})
	}
	for _, deviceId := range deviceIds {
		if _, ok := initState.initialized[deviceId]; ok {
			return nil, fmt.Errorf("device %d already initialized", deviceId)
		}
	}

	handle := utlzSamplerCreate(ids, int32(len(ids)), metricsCsv, int32(interval.Milliseconds()))
	if handle == nil {
		return nil, ErrSamplerCouldNotInitialize
	}

	s := &Sampler{handle: handle}

	initErr := ErrSamplerCouldNotInitialize
	for i, deviceId := range ids {
		if utlzSamplerIsInitialized(handle, deviceId) == 1 {
			s.initializedDeviceIDs = append(s.initializedDeviceIDs, deviceIds[i])
		} else {
			errStr := utlzSamplerGetError(handle, deviceId)
			initErr = errors.Join(initErr, fmt.Errorf("device %d could not initialize: %s", deviceId, errStr))
		}
	}

	if len(s.initializedDeviceIDs) == 0 {
		s.Close()
		return nil, initErr
	}

	for _, deviceId := range s.initializedDeviceIDs {
		initState.initialized[deviceId] = struct{}{}
	}

	s.metricCount = utlzSamplerGetMetricCount(handle)
	s.metricIndexByName = make(map[string]int32)
	for i := int32(0); i < s.metricCount; i++ {
		name := utlzSamplerGetMetricName(handle, i)
		s.metricIndexByName[name] = i
	}
	s.polledValues = make([]float64, s.metricCount)
	return s, nil
}

func (s *Sampler) InitializedDeviceIDs() []int {
	return s.initializedDeviceIDs
}

func (s *Sampler) Poll(deviceId int) (Snapshot, error) {
	if s.handle == nil {
		return Snapshot{}, ErrSamplerNotInitialized
	}

	if s.metricCount == 0 {
		return Snapshot{}, ErrSamplerNoMetrics
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	drained := utlzSamplerPoll(s.handle, int32(deviceId), s.polledValues, s.metricCount, &s.pmSamples, &s.groupId, &s.groupCount)
	if drained <= 0 {
		return Snapshot{}, ErrSamplerPollEmpty
	}
	return s.buildSnapshot(deviceId), nil
}

func (s *Sampler) metricValue(metric string) (float64, bool) {
	i, ok := s.metricIndexByName[metric]
	if !ok {
		return 0, false
	}
	v := s.polledValues[i]
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

func (s *Sampler) buildSnapshot(deviceId int) Snapshot {
	pipes := make(map[string]float64, len(Pipes))
	var computeSolPct, memorySolPct *float64
	for _, p := range Pipes {
		v, ok := s.metricValue(p.Metric)
		if !ok {
			continue
		}
		pipes[p.Name] = v
		switch p.Family {
		case PipeFamilyCompute:
			if computeSolPct == nil || v > *computeSolPct {
				val := v
				computeSolPct = &val
			}
		case PipeFamilyMemory:
			if memorySolPct == nil || v > *memorySolPct {
				val := v
				memorySolPct = &val
			}
		}
	}

	var smActivePct *float64
	if v, ok := s.metricValue(metricSMCyclesActive); ok {
		smActivePct = &v
	}

	return Snapshot{
		DeviceID:      deviceId,
		ComputeSOLPct: computeSolPct,
		MemorySOLPct:  memorySolPct,
		SMActivePct:   smActivePct,
		Pipes:         pipes,
		Timestamp:     time.Now(),
	}
}

func (s *Sampler) Close() error {
	if s.handle == nil {
		return nil
	}
	// TODO: get destroy error somehow?
	utlzSamplerDestroy(s.handle)
	s.handle = nil

	initState.mu.Lock()
	for _, deviceId := range s.initializedDeviceIDs {
		delete(initState.initialized, deviceId)
	}
	initState.mu.Unlock()

	return nil
}
