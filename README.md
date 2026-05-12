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

### Attainable SOL

Utilyze discovers running inference servers to detect which model is loaded on each GPU. It computes an attainable compute SOL ceiling (your realistic peak given that model and hardware).

Currently Utilyze only supports vLLM as a backend, with more (e.g. SGLang) coming soon. We are expanding model and hardware coverage over time; at present we support a subset of models on H100-80G and A100-80G GPUs within a node (up to 8 GPUs).

To enable this, Utilyze anonymously sends GPU configuration data to Systalyze's servers. Disable with `UTLZ_DISABLE_METRICS=1`.

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
- `UTLZ_BACKEND_URL`: set the backend URL for Systalyze's roofline SOL metrics API (default: `https://api.systalyze.com/v1/utilyze`)
- `UTLZ_DISABLE_METRICS`: disable workload detection and Systalyze roofline SOL metrics API

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

# build a runtime container image (utlz + CUPTI libs in nvidia/cuda base)
make image-runtime
# override the tag with IMAGE_NAME / IMAGE_TAG:
#   IMAGE_NAME=registry.example.com/utilyze IMAGE_TAG=$(git rev-parse --short HEAD) make image-runtime
```

The `image-runtime` target produces a local image suitable for running utilyze as a Kubernetes DaemonSet. For CI-driven multi-arch builds with push, invoke `docker buildx build --target runtime` directly with `--platform linux/amd64,linux/arm64 --push` and a registry-qualified tag.

There is experimental support for ARM64 builds using the sbsa-linux CUDA target.
