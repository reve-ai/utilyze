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
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/systalyze/utilyze/internal/version"
)

// meterName is the instrumentation scope. Used for the OTEL Meter() lookup
// and stored on every emitted metric.
const meterName = "github.com/systalyze/utilyze"

// solBucketBoundaries provides 14 buckets across the 0–100% range, weighted
// toward useful resolution at both extremes (GPU pipes tend to be either idle
// or near saturation; mid-range is less interesting analytically).
var solBucketBoundaries = []float64{1, 5, 10, 20, 30, 40, 50, 60, 70, 80, 90, 95, 99}

// OTELExporterConfig configures the OTLP metrics exporter. GpuNames and
// GpuUUIDs are keyed by physical device ID (matching MetricsSnapshot.GPUs).
type OTELExporterConfig struct {
	ClientID string
	GpuNames map[int]string
	GpuUUIDs map[int]string
}

// OTELExporter publishes per-GPU SOL metrics over OTLP. It emits each metric
// in two forms — see "Metrics" below — and is a no-op until UTLZ_OTEL_ENABLED=1.
// Configuration follows the standard OTEL_EXPORTER_OTLP_* environment variables.
//
// # Metrics
//
// For each underlying measurement we emit two instruments:
//
//   - A Float64ObservableGauge with the last observed value at export time
//     (suffix: bare metric name, e.g. utlz.gpu.sol.compute.pct). Best for
//     "what is the GPU doing right now" Grafana panels.
//
//   - A Float64Histogram aggregating every 250 ms sample within the export
//     window into explicit buckets (suffix: .distribution, e.g.
//     utlz.gpu.sol.compute.pct.distribution). Best for window-aggregated
//     analysis — fusion opportunity mining, p95/p99 SOL queries, etc.
//
// Metric names:
//
//	utlz.gpu.sol.compute.pct[.distribution]   max of compute pipes (0–100)
//	utlz.gpu.sol.memory.pct[.distribution]    max of memory pipes  (0–100)
//	utlz.gpu.sol.pipe.pct[.distribution]      per-pipe breakdown   (0–100)
//	utlz.gpu.sm.active.pct[.distribution]     DCGM-style sm_active (0–100)
//
// All metrics carry gpu.index, gpu.model, gpu.uuid attributes. The
// utlz.gpu.sol.pipe.pct metric additionally carries pipe= one of
// tensor|fma|alu|lsu_inst|issue|dram|l1tex.
//
// # Temporality
//
// Histograms (and counters, if any are added later) are exported with delta
// temporality so each export row reflects only that export window. This is the
// natural fit for ClickHouse-backed analysis where each row is queried
// independently with quantilesExact() or similar. Gauges have no temporality.
//
// # Sampling vs export cadence
//
// The native sampler polls at 250 ms regardless of export cadence. With a 30s
// OTEL export interval, each histogram bucket aggregates ~120 samples per series.
// The export interval is configurable via UTLZ_OTEL_EXPORT_INTERVAL (Go duration)
// or OTEL_METRIC_EXPORT_INTERVAL (integer milliseconds, spec standard).
type OTELExporter struct {
	provider *sdkmetric.MeterProvider

	// Synchronous histograms — Record() called on every Observe(snapshot).
	histComputeSOL metric.Float64Histogram
	histMemorySOL  metric.Float64Histogram
	histPipeSOL    metric.Float64Histogram
	histSMActive   metric.Float64Histogram

	// Latest snapshot per GPU — read by the async gauge callback at export time.
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
	// and OTEL_RESOURCE_ATTRIBUTES can override.
	res, err := resource.New(ctx,
		resource.WithSchemaURL(semconv.SchemaURL),
		resource.WithAttributes(
			semconv.ServiceName("utilyze"),
			semconv.ServiceVersion(version.VERSION),
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

	meter := provider.Meter(meterName,
		metric.WithInstrumentationVersion(version.VERSION),
		metric.WithSchemaURL(semconv.SchemaURL),
	)
	if err := e.registerInstruments(meter); err != nil {
		_ = provider.Shutdown(ctx)
		return nil, fmt.Errorf("register otel instruments: %w", err)
	}
	return e, nil
}

// Observe records the snapshot. Two things happen:
//   - Latest snapshot per GPU is cached for the async gauge callback to read
//     at the next periodic export.
//   - Each measurement in the snapshot is recorded into its histogram, which
//     the SDK aggregates into the configured buckets until the next export.
//
// Safe for concurrent calls.
func (e *OTELExporter) Observe(snapshot MetricsSnapshot) {
	e.mu.Lock()
	for _, gpu := range snapshot.GPUs {
		e.latest[gpu.DeviceID] = gpu
	}
	e.mu.Unlock()

	ctx := context.Background()
	for _, gpu := range snapshot.GPUs {
		base := e.baseAttrs(gpu.DeviceID)
		baseOpt := metric.WithAttributes(base...)

		if gpu.SOL.Valid {
			e.histComputeSOL.Record(ctx, gpu.SOL.ComputePct, baseOpt)
			e.histMemorySOL.Record(ctx, gpu.SOL.MemoryPct, baseOpt)
			for name, v := range gpu.SOL.Pipes {
				pipeAttrs := append(append([]attribute.KeyValue{}, base...), attribute.String("pipe", name))
				e.histPipeSOL.Record(ctx, v, metric.WithAttributes(pipeAttrs...))
			}
		}
		if gpu.DCGMUtilization.Valid {
			e.histSMActive.Record(ctx, gpu.DCGMUtilization.SMActivePct, baseOpt)
		}
	}
}

// Shutdown flushes pending exports and tears down the reader.
func (e *OTELExporter) Shutdown(ctx context.Context) error {
	if e == nil || e.provider == nil {
		return nil
	}
	return e.provider.Shutdown(ctx)
}

func (e *OTELExporter) registerInstruments(meter metric.Meter) error {
	// === Gauges: last-observed values, for live-state queries ===
	computeSOL, err := meter.Float64ObservableGauge("utlz.gpu.sol.compute.pct",
		metric.WithDescription("Compute SOL (max of compute pipes), 0-100. Last observed value at export time."),
		metric.WithUnit("%"),
	)
	if err != nil {
		return err
	}

	memorySOL, err := meter.Float64ObservableGauge("utlz.gpu.sol.memory.pct",
		metric.WithDescription("Memory SOL (max of memory pipes), 0-100. Last observed value at export time."),
		metric.WithUnit("%"),
	)
	if err != nil {
		return err
	}

	pipeSOL, err := meter.Float64ObservableGauge("utlz.gpu.sol.pipe.pct",
		metric.WithDescription("Per-pipe pct_of_peak_sustained_elapsed (0-100). Last observed value. pipe= one of tensor|fma|alu|lsu_inst|issue|dram|l1tex."),
		metric.WithUnit("%"),
	)
	if err != nil {
		return err
	}

	smActive, err := meter.Float64ObservableGauge("utlz.gpu.sm.active.pct",
		metric.WithDescription("DCGM-style sm__cycles_active (0-100). Last observed value at export time."),
		metric.WithUnit("%"),
	)
	if err != nil {
		return err
	}

	if _, err := meter.RegisterCallback(func(_ context.Context, obs metric.Observer) error {
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
		}
		return nil
	}, computeSOL, memorySOL, pipeSOL, smActive); err != nil {
		return err
	}

	// === Histograms: explicit-bucket distributions over the export window ===
	if e.histComputeSOL, err = meter.Float64Histogram("utlz.gpu.sol.compute.pct.distribution",
		metric.WithDescription("Distribution of Compute SOL (max of compute pipes) over the export window."),
		metric.WithUnit("%"),
		metric.WithExplicitBucketBoundaries(solBucketBoundaries...),
	); err != nil {
		return err
	}

	if e.histMemorySOL, err = meter.Float64Histogram("utlz.gpu.sol.memory.pct.distribution",
		metric.WithDescription("Distribution of Memory SOL (max of memory pipes) over the export window."),
		metric.WithUnit("%"),
		metric.WithExplicitBucketBoundaries(solBucketBoundaries...),
	); err != nil {
		return err
	}

	if e.histPipeSOL, err = meter.Float64Histogram("utlz.gpu.sol.pipe.pct.distribution",
		metric.WithDescription("Distribution of per-pipe pct_of_peak_sustained_elapsed over the export window. pipe= one of tensor|fma|alu|lsu_inst|issue|dram|l1tex."),
		metric.WithUnit("%"),
		metric.WithExplicitBucketBoundaries(solBucketBoundaries...),
	); err != nil {
		return err
	}

	if e.histSMActive, err = meter.Float64Histogram("utlz.gpu.sm.active.pct.distribution",
		metric.WithDescription("Distribution of DCGM-style sm__cycles_active over the export window."),
		metric.WithUnit("%"),
		metric.WithExplicitBucketBoundaries(solBucketBoundaries...),
	); err != nil {
		return err
	}

	return nil
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

// deltaTemporalitySelector forces delta temporality for cumulative-by-default
// instruments (histograms, counters). Gauges are unaffected (they have no
// temporality). Delta means each export row reflects only that export window
// rather than process-lifetime accumulation — simpler downstream queries in
// ClickHouse / Grafana.
func deltaTemporalitySelector(kind sdkmetric.InstrumentKind) metricdata.Temporality {
	switch kind {
	case sdkmetric.InstrumentKindHistogram,
		sdkmetric.InstrumentKindCounter,
		sdkmetric.InstrumentKindObservableCounter,
		sdkmetric.InstrumentKindUpDownCounter,
		sdkmetric.InstrumentKindObservableUpDownCounter:
		return metricdata.DeltaTemporality
	default:
		return metricdata.CumulativeTemporality
	}
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
		return otlpmetrichttp.New(ctx,
			otlpmetrichttp.WithTemporalitySelector(deltaTemporalitySelector),
		)
	case "", "grpc":
		return otlpmetricgrpc.New(ctx,
			otlpmetricgrpc.WithTemporalitySelector(deltaTemporalitySelector),
		)
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
