# syntax=docker/dockerfile:1.7
# A few notes here:
# - builds on rockylinux:8 for glibc 2.28+ compat
# - uses gcc-12 for better C++17 support than rockylinux:8 default gcc-8
# - NVIDIA provides 64-bit desktop ARM support under sbsa not aarch64/arm64
# - Go toolchain is pulled from the official `golang` image (the `go` binary
#   is statically built and runs on any Linux distro)
#
# Build targets:
#   --target export    binary tarball only (used by `make dist-tarball-docker`)
#   --target runtime   full runtime image with utlz + CUPTI host/target libs

ARG CUDA_VERSION=13.1
ARG CUDA_PKG_SUFFIX=13-1
ARG GCC_TOOLSET=gcc-toolset-12
ARG GO_VERSION=1.25
ARG RUNTIME_BASE=nvidia/cuda:13.1.0-base-ubuntu22.04

FROM rockylinux:8 AS builder

ARG TARGETARCH
ARG CUDA_VERSION
ARG CUDA_PKG_SUFFIX
ARG GCC_TOOLSET
ARG GO_VERSION

ENV PATH=/opt/rh/${GCC_TOOLSET}/root/usr/bin:/usr/local/cuda-${CUDA_VERSION}/bin:/usr/local/go/bin:${PATH} \
    LD_LIBRARY_PATH=/opt/rh/${GCC_TOOLSET}/root/usr/lib64

RUN set -eux; \
    case "$TARGETARCH" in \
      amd64) nv_arch=x86_64 ;; \
      arm64) nv_arch=sbsa ;; \
      *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    dnf -y install 'dnf-command(config-manager)'; \
    dnf config-manager --add-repo "https://developer.download.nvidia.com/compute/cuda/repos/rhel8/${nv_arch}/cuda-rhel8.repo"; \
    dnf -y install \
      "${GCC_TOOLSET}-gcc" \
      "${GCC_TOOLSET}-gcc-c++" \
      "${GCC_TOOLSET}-binutils" \
      gzip \
      make \
      tar \
      "cuda-driver-devel-${CUDA_PKG_SUFFIX}" \
      "cuda-cudart-devel-${CUDA_PKG_SUFFIX}" \
      "cuda-cupti-${CUDA_PKG_SUFFIX}"; \
    dnf clean all; \
    rm -rf /var/cache/dnf

# Bring in the Go toolchain. The `go` binary is statically linked, so the
# Debian-built tree runs fine on this rocky 8 builder.
COPY --from=golang:${GO_VERSION} /usr/local/go /usr/local/go

WORKDIR /src
COPY . .

RUN set -eux; \
    make clean; \
    make -j"$(nproc)" \
      CUDA_VERSION="${CUDA_VERSION}" \
      all dist-tarball

FROM scratch AS export
COPY --from=builder /src/dist/libutlz_sampler-*.tar.gz /

FROM ${RUNTIME_BASE} AS runtime
ARG CUDA_PKG_SUFFIX

# CUPTI host/target libs (libnvperf_host.so, libnvperf_target.so) are required
# at runtime by the embedded native sampler. ca-certificates kept for OTLP-over-TLS.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      "cuda-cupti-${CUDA_PKG_SUFFIX}" \
      ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# CUPTI libs land under /usr/local/cuda-*/extras/CUPTI/lib64 on the cuda base
# image; utlz's CUPTI loader already searches there (see internal/ffi/cupti).
COPY --from=builder /src/dist/utlz-linux-* /usr/local/bin/utlz
RUN chmod +x /usr/local/bin/utlz

# OTEL config is supplied via env at runtime (UTLZ_OTEL_ENABLED,
# OTEL_EXPORTER_OTLP_ENDPOINT, ...). Profiling requires CAP_SYS_ADMIN unless
# the host sets NVreg_RestrictProfilingToAdminUsers=0.
ENTRYPOINT ["/usr/local/bin/utlz"]
