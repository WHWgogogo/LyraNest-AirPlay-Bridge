# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# lyranest-airplay-bridge
#
# Multi-stage build. The final image carries exactly two binaries - the bridge
# and the audio push kernel - plus ffmpeg for decoding. No Go toolchain, no
# build cache, no source.
#
# Network mode: this image MUST run with host networking. AirPlay needs mDNS
# multicast (224.0.0.251:5353) and the receiver dials back to a random UDP
# timing port on the sender, neither of which survives a bridge network.
# ---------------------------------------------------------------------------

ARG GO_IMAGE=golang:1.24-alpine
ARG RUNTIME_IMAGE=alpine:3.20

# ---------------------------------------------------------------------------
# Stage 1: build the bridge
# ---------------------------------------------------------------------------
FROM ${GO_IMAGE} AS build

WORKDIR /src

# Dependencies first so the module cache survives source edits.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY cmd/ cmd/
COPY internal/ internal/

ARG TARGETOS=linux
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/airplay-bridge ./cmd/bridge

# The virtual AirPlay receiver is a verification tool, not a runtime
# dependency. It is built separately so it can be shipped to a test host.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/mockreceiver ./cmd/mockreceiver

# ---------------------------------------------------------------------------
# Stage 2: runtime base
# ---------------------------------------------------------------------------
FROM ${RUNTIME_IMAGE} AS runtime-base

LABEL org.opencontainers.image.title="LyraNest AirPlay Bridge" \
      org.opencontainers.image.version="0.1.0"

RUN apk add --no-cache ca-certificates ffmpeg tzdata \
    && update-ca-certificates

COPY --from=build /out/airplay-bridge /usr/local/bin/airplay-bridge

ENV BRIDGE_PORT=8092 \
    BRIDGE_BIND=0.0.0.0 \
    BRIDGE_ENGINE=auto \
    CLIAIRPLAY_PATH=cliairplay \
    FFMPEG_PATH=ffmpeg \
    BRIDGE_DEVICE_TTL_SECONDS=60 \
    BRIDGE_LOG_LEVEL=info \
    LYRANEST_SERVER_URL=http://127.0.0.1:8080

EXPOSE 8092

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O - "http://127.0.0.1:${BRIDGE_PORT}/healthz" >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/usr/local/bin/airplay-bridge"]

# ---------------------------------------------------------------------------
# Stage 3a (default): bundle the mature cliairplay push kernel
#
# The upstream Music Assistant kernel is the recommended engine for real
# HomePods: it already absorbs the AirPlay 2 native flow's firmware quirks
# (HAP transient pairing, encrypted RTSP, ChaCha20-Poly1305 audio, PTP).
#
# It is fetched from the pinned upstream release and verified by SHA256. It
# runs as a separate process, so its GPL-3.0 licence does not reach this
# image's own code.
# ---------------------------------------------------------------------------
FROM runtime-base AS runtime

ARG TARGETARCH
ARG CLIAIRPLAY_VERSION=v0.5.4
ARG WITH_CLIAIRPLAY=1

RUN set -eux; \
    if [ "${WITH_CLIAIRPLAY}" != "1" ]; then \
        echo "WITH_CLIAIRPLAY=${WITH_CLIAIRPLAY}: shipping the built-in native kernel only"; \
        exit 0; \
    fi; \
    case "${TARGETARCH:-amd64}" in \
        amd64) asset="cliairplay-linux-x86_64";  sha="1ac56a15fb548f07a1dae94be16d4bea308420a0ec0cd04238023e44345fc1c9" ;; \
        arm64) asset="cliairplay-linux-aarch64"; sha="1ce9160a8a9abb1b2263dfbf5ed59551dd1205b2336a024e9e33c903c201a310" ;; \
        *) echo "no cliairplay build for ${TARGETARCH}; rebuild with --build-arg WITH_CLIAIRPLAY=0 to use the native kernel" >&2; exit 1 ;; \
    esac; \
    wget -q -O /usr/local/bin/cliairplay \
        "https://github.com/music-assistant/airplay-cli/releases/download/${CLIAIRPLAY_VERSION}/${asset}"; \
    echo "${sha}  /usr/local/bin/cliairplay" | sha256sum -c -; \
    chmod 0755 /usr/local/bin/cliairplay; \
    /usr/local/bin/cliairplay --check || echo "warning: cliairplay --check did not run under this build platform"

# ---------------------------------------------------------------------------
# Stage 3b: pure-Go image, no external kernel
# ---------------------------------------------------------------------------
FROM runtime-base AS runtime-native

ENV BRIDGE_ENGINE=native

