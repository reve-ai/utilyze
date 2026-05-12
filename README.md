# Utilyze

Utilyze measures how efficiently your GPU is doing useful work, not just whether it's busy. It runs live against your workload with negligible overhead.

![utlz in action](./assets/utlz.png)

Standard tools like `nvidia-smi` and `nvtop` only check whether a kernel is running on the GPU. They can show 100% while your workload is using a tiny fraction of the hardware's real capacity. 

Utilyze reads GPU performance counters directly to show what's actually being used, and provides an estimate of how far you can push utilization given a workload, model, and hardware. To learn more, read [our blog post](https://systalyze.com/utilyze).

Utilyze is created by [Systalyze](https://systalyze.com).

**Read this in other languages:** [中文](./README.zh-CN.md)

## Requirements

- Linux amd64 (arm64 support coming soon)
- NVIDIA Ampere or newer GPU (A100, H100, H200, B200, RTX 3000+)
- CUDA Toolkit 11.0+
- `sudo` or `CAP_SYS_ADMIN` (see below), or privileged container

## Installation

```bash
# macOS/Linux
curl -sSfL https://systalyze.com/utilyze/install.sh | sh

# Windows
iex (curl.exe -L https://systalyze.com/utilyze/install.ps1 | Out-String)
```

For macOS and Windows versions, **Utilyze acts as a client for another Utilyze process running on a remote Linux machine with profiling capabilities.** These do not require root nor any native libraries. On Windows, you may need to add an exception to executable path for Windows Defender and then reinstall Utilyze:

```powershell
Add-MpPreference -ExclusionPath <INSTALL_DIR>
iex (curl.exe -L https://systalyze.com/utilyze/install.ps1 | Out-String)
```

Utilyze will likely require root for profiling capabilities depending on your host configuration (see below) and will prompt you for your password during installation to install it system-wide.

If CUPTI 12+ is not found, `utlz` will prompt you to install the latest release from PyPI on first run.

## Usage

On a Linux machine with profiling capabilities, you can:
```bash
# monitor all GPUs for SOL metrics
sudo utlz

# monitor specific GPUs
sudo utlz --devices 0,2

# show discovered inference server endpoints per GPU
sudo utlz --endpoints
```
This starts a WebSocket server that listens for connections from other Utilyze processes on port 8079 by default. Further instances will automatically connect to the same server.

On a macOS/Windows machine, you can connect to a running server with:
```bash
utlz --connect <SERVER_URL>
```

Note that a single device ID can only be monitored by a single instance of `utlz`. This is due to the way NVIDIA's Perf SDK API handles device access.

### Exporting metrics via OpenTelemetry

Utilyze can export per-GPU SOL metrics to an OpenTelemetry collector over OTLP (gRPC by default, HTTP optional). The exporter is off by default — enable it with `UTLZ_OTEL_ENABLED=1`. All standard `OTEL_EXPORTER_OTLP_*` environment variables are honored via the OpenTelemetry SDK.

```bash
sudo -E UTLZ_OTEL_ENABLED=1 \
    OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317 \
    OTEL_EXPORTER_OTLP_INSECURE=true \
    OTEL_METRIC_EXPORT_INTERVAL=10000 \
    utlz
```

`sudo -E` matters — `sudo` strips environment variables by default, so `UTLZ_OTEL_ENABLED` would otherwise be lost and the exporter would stay disabled. Alternatively, set `NVreg_RestrictProfilingToAdminUsers=0` on the host (see [Running without sudo](#running-without-sudo)) so `utlz` doesn't need `sudo` at all.

Each gauge carries `gpu.index`, `gpu.model`, and `gpu.uuid` attributes. See [Metrics reference](#metrics-reference) for the full list.

#### OTEL configuration

| Variable | Purpose | Default |
|---|---|---|
| `UTLZ_OTEL_ENABLED` | Master switch; set to `1` to enable export | off |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP collector endpoint | `localhost:4317` |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `grpc` or `http/protobuf` | `grpc` |
| `OTEL_EXPORTER_OTLP_INSECURE` | Skip TLS | `false` |
| `OTEL_EXPORTER_OTLP_HEADERS` | Auth headers | — |
| `OTEL_METRIC_EXPORT_INTERVAL` | Export period in milliseconds (OTEL spec) | 60000 |
| `UTLZ_OTEL_EXPORT_INTERVAL` | Export period as Go duration (e.g. `10s`); wins over the above | — |
| `OTEL_SERVICE_NAME` | Service name resource attribute | `utilyze` |
| `OTEL_RESOURCE_ATTRIBUTES` | Additional resource attributes (e.g. `k8s.node.name=...`) | — |

Sampling cadence is independent of export cadence: the native sampler polls at 250 ms regardless. The exporter uses last-observed semantics, so each gauge reports the most recent 250 ms sample's value at export time — use `avg_over_time(...)` or similar at query time in your TSDB if you want windowed aggregates.

### Running without sudo

By default, NVIDIA restricts GPU profiling counters to admin users. To allow non-root access, disable the restriction on the host and reboot:

```bash
echo 'options nvidia NVreg_RestrictProfilingToAdminUsers=0' | sudo tee /etc/modprobe.d/nvidia-profiling.conf
sudo reboot
```

After this, `utlz` can run without sudo. If `utlz` warns about missing capabilities, you can disable the warning via `UTLZ_DISABLE_PROFILING_WARNING=1` (see Options).

### Options

Flags (most have environment variable equivalents):

- `--endpoints`: show discovered inference server endpoints per GPU
- `--devices` / `UTLZ_DEVICES`: monitor specific GPUs (comma-separated list of device IDs)
- `--log` / `UTLZ_LOG`: a file to write logs to (default: no logging)
- `--log-level` / `UTLZ_LOG_LEVEL`: set the log level (default: `INFO`, other options: `DEBUG`, `WARN`, `ERROR`)
- `--version`: show the version

Environment variables only:

- `UTLZ_DISABLE_PROFILING_WARNING`: disable the warning about GPU profiling capabilities on startup

For OpenTelemetry-related variables see [OTEL configuration](#otel-configuration).

## Metrics reference

When OTEL export is enabled (see [Exporting metrics via OpenTelemetry](#exporting-metrics-via-opentelemetry)), Utilyze emits four gauges per GPU per scrape — 10 datapoints total. Every value is in `pct_of_peak_sustained_elapsed` units (0–100). Every datapoint carries `gpu.index`, `gpu.model`, and `gpu.uuid` attributes.

| Metric name | Type | Description |
|---|---|---|
| `utlz.gpu.sol.compute.pct` | Float64 gauge | Compute SOL — max of compute pipes (`tensor`, `fma`, `alu`, `lsu_inst`, `issue`) |
| `utlz.gpu.sol.memory.pct` | Float64 gauge | Memory SOL — max of memory pipes (`dram`, `l1tex`) |
| `utlz.gpu.sol.pipe.pct` | Float64 gauge | Per-pipe breakdown; additional `pipe=` attribute identifies the pipe |
| `utlz.gpu.sm.active.pct` | Float64 gauge | DCGM-style `sm__cycles_active` — overall SM-busy fraction |

The compute and memory roll-ups are strictly redundant with `utlz.gpu.sol.pipe.pct` and are provided for query ergonomics (e.g. one-shot Grafana panels). To save series cardinality, you can recompute them at query time and drop the roll-up gauges.

### `utlz.gpu.sol.pipe.pct` — pipe attribute

The `pipe=` attribute on `utlz.gpu.sol.pipe.pct` maps 1:1 to an underlying NVPerf counter:

| `pipe=` | Underlying NVPerf metric | What it represents |
|---|---|---|
| `tensor` | `sm__pipe_tensor_cycles_active.avg.pct_of_peak_sustained_elapsed` | Tensor core / matmul throughput |
| `fma` | `sm__pipe_fma_cycles_active.avg.pct_of_peak_sustained_elapsed` | FP FMA pipe (scalar/vector floating-point math) |
| `alu` | `sm__pipe_alu_cycles_active.avg.pct_of_peak_sustained_elapsed` | Integer / logical ALU pipe |
| `lsu_inst` | `sm__inst_executed_pipe_lsu.avg.pct_of_peak_sustained_elapsed` | LSU instruction issue rate (load/store pipe busy) |
| `issue` | `sm__issue_active.avg.pct_of_peak_sustained_elapsed` | Warp scheduler issue rate |
| `dram` | `dram__throughput.avg.pct_of_peak_sustained_elapsed` | HBM bandwidth |
| `l1tex` | `l1tex__data_pipe_lsu_wavefronts.avg.pct_of_peak_sustained_elapsed` | L1 cache bandwidth |

The first five contribute to Compute SOL; the last two contribute to Memory SOL.

### Example PromQL

```promql
# Dominant compute pipe per GPU over the last 5 minutes
topk(1,
  avg_over_time(utlz_gpu_sol_pipe_pct{pipe=~"tensor|fma|alu|lsu_inst|issue"}[5m])
) by (gpu_uuid)

# Fleet-wide tensor-pipe underutilization (low tensor% with high compute SOL → fusion candidate)
avg_over_time(utlz_gpu_sol_compute_pct[5m]) - avg_over_time(utlz_gpu_sol_pipe_pct{pipe="tensor"}[5m])
```

## Build from source

To build from source you'll need:

- Go 1.25+ for the CLI
- Docker for building the native library with wide compatibility
- CUDA Toolkit (13.1 is linked against by default but can be set via `CUDA_VERSION`)

```bash
# build the native library and the CLI
make all

# build and package the native library via Docker
make dist-tarball-docker

# build the CLI only
make utlz
```

There is experimental support for ARM64 builds using the sbsa-linux CUDA target.
