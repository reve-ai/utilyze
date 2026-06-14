package metrics

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/systalyze/utilyze/internal/ffi/nvml"
)

// throttleReasons enumerates the actionable clock-throttle bits decoded onto the
// utlz.gpu.clocks.throttle gauge via the reason= attribute (0/1 per reason).
var throttleReasons = []struct {
	name string
	bit  uint64
}{
	{"sw_power_cap", nvml.ThrottleReasonSwPowerCap},
	{"hw_slowdown", nvml.ThrottleReasonHwSlowdown},
	{"sw_thermal", nvml.ThrottleReasonSwThermal},
	{"hw_thermal", nvml.ThrottleReasonHwThermal},
	{"hw_power_brake", nvml.ThrottleReasonHwPowerBrake},
}

// OTELExporterConfig configures the OTLP metrics exporter. GpuNames and
// GpuUUIDs are keyed by physical device ID (matching MetricsSnapshot.GPUs).
type OTELExporterConfig struct {
	ClientID string
	GpuNames map[int]string
	GpuUUIDs map[int]string
}

// OTELExporter publishes per-GPU SOL metrics (rolled-up and per-pipe) over OTLP.
// It is a no-op until UTLZ_OTEL_ENABLED=1; configuration follows the standard
// OTEL_EXPORTER_OTLP_* environment variables.
//
// Metrics:
//   - utlz.gpu.sol.compute.pct  — max of compute pipes (0–100)
//   - utlz.gpu.sol.memory.pct   — max of memory pipes  (0–100)
//   - utlz.gpu.sol.pipe.pct     — per-pipe breakdown with pipe= attribute
//   - utlz.gpu.sm.active.pct    — DCGM-style sm__cycles_active (0–100)
//   - utlz.gpu.pcie.tx.bps      — PCIe transmit throughput (bytes/sec)
//   - utlz.gpu.pcie.rx.bps      — PCIe receive throughput (bytes/sec)
//   - utlz.gpu.nvlink.tx.bps    — NVLink transmit throughput, summed over links (bytes/sec)
//   - utlz.gpu.nvlink.rx.bps    — NVLink receive throughput, summed over links (bytes/sec)
//   - utlz.gpu.util.pct         — NVML overall GPU utilization (nvidia-smi GPU-Util)
//   - utlz.gpu.mem.util.pct     — NVML memory-controller utilization
//   - utlz.gpu.temperature.celsius
//   - utlz.gpu.power.watts / utlz.gpu.power.limit.watts
//   - utlz.gpu.clock.sm.mhz / utlz.gpu.clock.mem.mhz
//   - utlz.gpu.mem.used.bytes / utlz.gpu.mem.total.bytes
//   - utlz.gpu.clocks.throttle  — per-reason 0/1 indicator with reason= attribute
//
// All metrics carry gpu.index, gpu.model, gpu.uuid attributes. The pipe gauge
// additionally carries pipe= one of tensor|fma|alu|lsu_inst|issue|dram|l1tex.
// The throttle gauge carries reason= one of sw_power_cap|hw_slowdown|sw_thermal|
// hw_thermal|hw_power_brake.
//
// Values use last-observed semantics: each gauge reports the most recent
// snapshot value at the time the OTEL reader collects (default every 10s, or
// OTEL_METRIC_EXPORT_INTERVAL via the SDK). Sampling cadence is unchanged
// (250ms in main.go), so spikes between exports are not smoothed in-process —
// use Prometheus / your TSDB for rate/avg_over_time aggregation.
type OTELExporter struct {
	provider *sdkmetric.MeterProvider

	mu     sync.Mutex
	latest map[int]GPUSnapshot

	gpuNames map[int]string
	gpuUUIDs map[int]string
}

// NewOTELExporter constructs an exporter and starts the periodic reader. The
// caller must call Shutdown to flush before exit. Honors the standard OTLP
// env vars (OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_EXPORTER_OTLP_HEADERS, ...).
func NewOTELExporter(ctx context.Context, cfg OTELExporterConfig) (*OTELExporter, error) {
	exporter, err := newOTLPMetricExporter(ctx)
	if err != nil {
		return nil, fmt.Errorf("otlp metric exporter: %w", err)
	}

	// Order matters: defaults first, then WithFromEnv last so OTEL_SERVICE_NAME
	// and OTEL_RESOURCE_ATTRIBUTES can override. Putting WithAttributes after
	// WithFromEnv (the previous arrangement) silently ignored the env vars.
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName("utilyze"),
			semconv.ServiceInstanceID(cfg.ClientID),
		),
		resource.WithHost(),
		resource.WithProcess(),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
	}

	readerOpts := []sdkmetric.PeriodicReaderOption{}
	if d := exportInterval(); d > 0 {
		readerOpts = append(readerOpts, sdkmetric.WithInterval(d))
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter, readerOpts...)),
	)

	e := &OTELExporter{
		provider: provider,
		latest:   make(map[int]GPUSnapshot),
		gpuNames: cfg.GpuNames,
		gpuUUIDs: cfg.GpuUUIDs,
	}

	meter := provider.Meter("github.com/systalyze/utilyze")
	if err := e.registerGauges(meter); err != nil {
		_ = provider.Shutdown(ctx)
		return nil, fmt.Errorf("register otel gauges: %w", err)
	}
	return e, nil
}

// Observe records the latest per-GPU snapshot. Safe for concurrent calls.
func (e *OTELExporter) Observe(snapshot MetricsSnapshot) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, gpu := range snapshot.GPUs {
		e.latest[gpu.DeviceID] = gpu
	}
}

// Shutdown flushes pending exports and tears down the reader.
func (e *OTELExporter) Shutdown(ctx context.Context) error {
	if e == nil || e.provider == nil {
		return nil
	}
	return e.provider.Shutdown(ctx)
}

func (e *OTELExporter) registerGauges(meter metric.Meter) error {
	computeSOL, err := meter.Float64ObservableGauge("utlz.gpu.sol.compute.pct",
		metric.WithDescription("Compute SOL (max of compute pipes), 0-100"),
		metric.WithUnit("1"),
	)
	if err != nil {
		return err
	}

	memorySOL, err := meter.Float64ObservableGauge("utlz.gpu.sol.memory.pct",
		metric.WithDescription("Memory SOL (max of memory pipes), 0-100"),
		metric.WithUnit("1"),
	)
	if err != nil {
		return err
	}

	pipeSOL, err := meter.Float64ObservableGauge("utlz.gpu.sol.pipe.pct",
		metric.WithDescription("Per-pipe pct_of_peak_sustained_elapsed (0-100). pipe= one of tensor|fma|alu|lsu_inst|issue|dram|l1tex."),
		metric.WithUnit("1"),
	)
	if err != nil {
		return err
	}

	smActive, err := meter.Float64ObservableGauge("utlz.gpu.sm.active.pct",
		metric.WithDescription("DCGM-style sm__cycles_active (0-100)"),
		metric.WithUnit("1"),
	)
	if err != nil {
		return err
	}

	pcieTx, err := meter.Float64ObservableGauge("utlz.gpu.pcie.tx.bps",
		metric.WithDescription("PCIe transmit throughput (bytes/sec)"),
		metric.WithUnit("By/s"),
	)
	if err != nil {
		return err
	}

	pcieRx, err := meter.Float64ObservableGauge("utlz.gpu.pcie.rx.bps",
		metric.WithDescription("PCIe receive throughput (bytes/sec)"),
		metric.WithUnit("By/s"),
	)
	if err != nil {
		return err
	}

	nvlinkTx, err := meter.Float64ObservableGauge("utlz.gpu.nvlink.tx.bps",
		metric.WithDescription("NVLink transmit throughput, summed over active links (bytes/sec)"),
		metric.WithUnit("By/s"),
	)
	if err != nil {
		return err
	}

	nvlinkRx, err := meter.Float64ObservableGauge("utlz.gpu.nvlink.rx.bps",
		metric.WithDescription("NVLink receive throughput, summed over active links (bytes/sec)"),
		metric.WithUnit("By/s"),
	)
	if err != nil {
		return err
	}

	gpuUtil, err := meter.Float64ObservableGauge("utlz.gpu.util.pct",
		metric.WithDescription("NVML overall GPU utilization, i.e. nvidia-smi GPU-Util (0-100)"),
		metric.WithUnit("1"),
	)
	if err != nil {
		return err
	}

	memUtil, err := meter.Float64ObservableGauge("utlz.gpu.mem.util.pct",
		metric.WithDescription("NVML memory-controller utilization (0-100)"),
		metric.WithUnit("1"),
	)
	if err != nil {
		return err
	}

	temperature, err := meter.Float64ObservableGauge("utlz.gpu.temperature.celsius",
		metric.WithDescription("GPU core temperature (degrees Celsius)"),
		metric.WithUnit("Cel"),
	)
	if err != nil {
		return err
	}

	power, err := meter.Float64ObservableGauge("utlz.gpu.power.watts",
		metric.WithDescription("GPU board power draw (watts)"),
		metric.WithUnit("W"),
	)
	if err != nil {
		return err
	}

	powerLimit, err := meter.Float64ObservableGauge("utlz.gpu.power.limit.watts",
		metric.WithDescription("Enforced GPU power limit (watts)"),
		metric.WithUnit("W"),
	)
	if err != nil {
		return err
	}

	smClock, err := meter.Float64ObservableGauge("utlz.gpu.clock.sm.mhz",
		metric.WithDescription("Current SM clock (MHz)"),
		metric.WithUnit("MHz"),
	)
	if err != nil {
		return err
	}

	memClock, err := meter.Float64ObservableGauge("utlz.gpu.clock.mem.mhz",
		metric.WithDescription("Current memory clock (MHz)"),
		metric.WithUnit("MHz"),
	)
	if err != nil {
		return err
	}

	memUsed, err := meter.Float64ObservableGauge("utlz.gpu.mem.used.bytes",
		metric.WithDescription("HBM memory in use (bytes)"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return err
	}

	memTotal, err := meter.Float64ObservableGauge("utlz.gpu.mem.total.bytes",
		metric.WithDescription("Total HBM memory (bytes)"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return err
	}

	throttle, err := meter.Float64ObservableGauge("utlz.gpu.clocks.throttle",
		metric.WithDescription("Clock throttle active (1) or not (0) per reason= attribute"),
		metric.WithUnit("1"),
	)
	if err != nil {
		return err
	}

	_, err = meter.RegisterCallback(func(_ context.Context, obs metric.Observer) error {
		e.mu.Lock()
		snapshots := make([]GPUSnapshot, 0, len(e.latest))
		for _, snap := range e.latest {
			snapshots = append(snapshots, snap)
		}
		e.mu.Unlock()

		for _, snap := range snapshots {
			base := e.baseAttrs(snap.DeviceID)
			if snap.SOL.Valid {
				obs.ObserveFloat64(computeSOL, snap.SOL.ComputePct, metric.WithAttributes(base...))
				obs.ObserveFloat64(memorySOL, snap.SOL.MemoryPct, metric.WithAttributes(base...))
				for name, v := range snap.SOL.Pipes {
					attrs := append(append([]attribute.KeyValue{}, base...), attribute.String("pipe", name))
					obs.ObserveFloat64(pipeSOL, v, metric.WithAttributes(attrs...))
				}
			}
			if snap.DCGMUtilization.Valid {
				obs.ObserveFloat64(smActive, snap.DCGMUtilization.SMActivePct, metric.WithAttributes(base...))
			}
			if snap.Bandwidth.Valid {
				obs.ObserveFloat64(pcieTx, snap.Bandwidth.PCIeTxBps, metric.WithAttributes(base...))
				obs.ObserveFloat64(pcieRx, snap.Bandwidth.PCIeRxBps, metric.WithAttributes(base...))
				obs.ObserveFloat64(nvlinkTx, snap.Bandwidth.NVLinkTxBps, metric.WithAttributes(base...))
				obs.ObserveFloat64(nvlinkRx, snap.Bandwidth.NVLinkRxBps, metric.WithAttributes(base...))
			}
			if snap.NVMLUtilization.Valid {
				obs.ObserveFloat64(gpuUtil, snap.NVMLUtilization.UtilPct, metric.WithAttributes(base...))
				obs.ObserveFloat64(memUtil, snap.NVMLUtilization.MemUtilPct, metric.WithAttributes(base...))
			}
			if snap.Health.Valid {
				h := snap.Health
				if h.TempC != nil {
					obs.ObserveFloat64(temperature, *h.TempC, metric.WithAttributes(base...))
				}
				if h.PowerW != nil {
					obs.ObserveFloat64(power, *h.PowerW, metric.WithAttributes(base...))
				}
				if h.PowerLimitW != nil {
					obs.ObserveFloat64(powerLimit, *h.PowerLimitW, metric.WithAttributes(base...))
				}
				if h.SMClockMHz != nil {
					obs.ObserveFloat64(smClock, *h.SMClockMHz, metric.WithAttributes(base...))
				}
				if h.MemClockMHz != nil {
					obs.ObserveFloat64(memClock, *h.MemClockMHz, metric.WithAttributes(base...))
				}
				if h.MemUsedBytes != nil {
					obs.ObserveFloat64(memUsed, *h.MemUsedBytes, metric.WithAttributes(base...))
				}
				if h.MemTotalBytes != nil {
					obs.ObserveFloat64(memTotal, *h.MemTotalBytes, metric.WithAttributes(base...))
				}
				if h.ThrottleReasons != nil {
					for _, tr := range throttleReasons {
						v := 0.0
						if *h.ThrottleReasons&tr.bit != 0 {
							v = 1
						}
						attrs := append(append([]attribute.KeyValue{}, base...), attribute.String("reason", tr.name))
						obs.ObserveFloat64(throttle, v, metric.WithAttributes(attrs...))
					}
				}
			}
		}
		return nil
	}, computeSOL, memorySOL, pipeSOL, smActive, pcieTx, pcieRx, nvlinkTx, nvlinkRx,
		gpuUtil, memUtil, temperature, power, powerLimit, smClock, memClock, memUsed, memTotal, throttle)
	return err
}

func (e *OTELExporter) baseAttrs(deviceID int) []attribute.KeyValue {
	attrs := []attribute.KeyValue{attribute.Int("gpu.index", deviceID)}
	if name := e.gpuNames[deviceID]; name != "" {
		attrs = append(attrs, attribute.String("gpu.model", name))
	}
	if uuid := e.gpuUUIDs[deviceID]; uuid != "" {
		attrs = append(attrs, attribute.String("gpu.uuid", uuid))
	}
	return attrs
}

// newOTLPMetricExporter picks gRPC by default, http/protobuf if explicitly
// requested via OTEL_EXPORTER_OTLP_PROTOCOL or OTEL_EXPORTER_OTLP_METRICS_PROTOCOL.
func newOTLPMetricExporter(ctx context.Context) (sdkmetric.Exporter, error) {
	proto := os.Getenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL")
	if proto == "" {
		proto = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	switch proto {
	case "http/protobuf", "http/json":
		return otlpmetrichttp.New(ctx)
	case "", "grpc":
		return otlpmetricgrpc.New(ctx)
	default:
		return nil, errors.New("unsupported OTEL_EXPORTER_OTLP_PROTOCOL=" + proto)
	}
}

// exportInterval picks the periodic reader interval. UTLZ_OTEL_EXPORT_INTERVAL
// (Go duration, e.g. "30s") wins if set; otherwise OTEL_METRIC_EXPORT_INTERVAL
// (integer milliseconds, per the OTEL spec) is honored. Returns 0 to use the
// SDK default (60s).
func exportInterval() time.Duration {
	if s := os.Getenv("UTLZ_OTEL_EXPORT_INTERVAL"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
	}
	if s := os.Getenv("OTEL_METRIC_EXPORT_INTERVAL"); s != "" {
		if ms, err := strconv.Atoi(s); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return 0
}
