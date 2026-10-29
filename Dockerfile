# syntax=docker/dockerfile:1

# ----------- stage: frontend
# Build the dashboard static export (arch-independent) on the native build
# platform. Bun needs a glibc base image, so this uses the official Bun image
# rather than the Alpine Go build stage; the output is copied into
# web/dashboard before the Go build so `go:embed` picks it up.
FROM --platform=$BUILDPLATFORM oven/bun:1.4.2 AS frontend
WORKDIR /frontend
COPY frontend/package.json frontend/bun.lock ./
RUN bun install --frozen-lockfile
COPY frontend/ ./
RUN bun run build

# ----------- stage: build
# The native SQLite driver requires CGO, so the build stage runs on the target
# platform with the target's C toolchain. Building the arm64 binary therefore
# needs binfmt/QEMU (docker/setup-qemu-action in CI), which is slower than the
# previous pure-Go cross-build but keeps a single, verifiable toolchain.
FROM --platform=$TARGETPLATFORM golang:alpine AS build
RUN apk add --no-cache make coreutils libcap gcc musl-dev

# Arguments needed for dependency download
ARG GOPROXY=""
ARG OPTS=""

# setup go environment
ENV GO_SKIP_GENERATE=1\
  GO_SKIP_DASHBOARD=1 \
  GO_BUILD_FLAGS="-tags netgo,osusergo,sqlite_omit_load_extension -v ${OPTS}" \
  EXTRA_LD_FLAGS="-linkmode external -extldflags -static" \
  CGO_ENABLED=1 \
  BIN_USER=100\
  BIN_AUTOCAB=1 \
  BIN_OUT_DIR="/bin" \
  GOPROXY=$GOPROXY

# Copy dependency files first for better caching
COPY go.mod go.sum ./

# Download dependencies (cached layer unless go.mod/go.sum change)
RUN go mod download

# Copy source code after dependencies are cached
COPY . .

# Embed the dashboard built by the frontend stage (the source tree only
# carries the gitignored placeholder). Copied after COPY . . so it always wins.
COPY --from=frontend /frontend/out ./web/dashboard/

# Arguments needed for build step only
ARG VERSION
ARG BUILD_TIME

# Target platform args populated automatically by BuildKit; cross-compile to them
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT

RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOARM=${TARGETVARIANT#v} make build

# Empty dir, copied into the final stage to seed writable mount points. Docker
# propagates the ownership of an existing image directory to a fresh named
# volume mounted over it, so pre-creating these makes `-v denyzer_cache:/app/cache`
# writable by the unprivileged container user without any host-side setup.
RUN mkdir -p /seed-dir

# ----------- stage: final
FROM scratch

ARG VERSION
ARG BUILD_TIME
ARG DOC_PATH

LABEL org.opencontainers.image.title="denyzer" \
  org.opencontainers.image.vendor="mia0x75" \
  org.opencontainers.image.licenses="Apache-2.0" \
  org.opencontainers.image.version="${VERSION}" \
  org.opencontainers.image.created="${BUILD_TIME}" \
  org.opencontainers.image.description="Fast and lightweight DNS proxy as ad-blocker for local network with many features" \
  org.opencontainers.image.url="https://github.com/mia0x75/denyzer#readme" \
  org.opencontainers.image.source="https://github.com/mia0x75/denyzer" \
  org.opencontainers.image.documentation="https://mia0x75.github.io/denyzer/${DOC_PATH}/"



USER 100
WORKDIR /app

COPY --from=build /bin/denyzer /app/denyzer

# Writable mount points owned by the container user (see comment in build stage).
COPY --from=build --chown=100:100 /seed-dir /app/cache
COPY --from=build --chown=100:100 /seed-dir /logs

ENV DENYZER_CONFIG_FILE=/app/config.yml

ENTRYPOINT ["/app/denyzer"]

HEALTHCHECK --start-period=1m --timeout=3s CMD ["/app/denyzer", "healthcheck"]
