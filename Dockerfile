# syntax=docker/dockerfile:1.7

# ---------- build ----------
FROM golang:1.27-alpine AS builder
WORKDIR /src

ARG VERSION=dev
ENV CGO_ENABLED=0 GOFLAGS=-buildvcs=false

# go.sum* glob: a dependency-free module has no go.sum yet.
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/edgecache ./cmd/edgecache \
    && go build -trimpath -ldflags="-s -w" -o /out/slow-origin ./cmd/slow-origin

# ---------- slow-origin (test origin for the compose smoke test; not deployed) ----------
# docker build --target slow-origin -t edgecache-slow-origin .
FROM gcr.io/distroless/static-debian12:nonroot AS slow-origin
COPY --from=builder /out/slow-origin /slow-origin
ENV ORIGIN_ADDR=0.0.0.0:8082
EXPOSE 8082
USER nonroot:nonroot
ENTRYPOINT ["/slow-origin"]

# ---------- runtime (default target: the last stage) ----------
# distroless/static: no shell, no package manager, runs as uid 65532 (nonroot).
# NOTE: HEALTHCHECK is intentionally absent -- distroless has no shell/curl/wget to run it
# and Kubernetes/compose should probe GET /healthz and /readyz over HTTP instead.
FROM gcr.io/distroless/static-debian12:nonroot AS edgecache

COPY --from=builder /out/edgecache /edgecache

# The proxy listener must bind every interface inside the container; the
# admin listener stays on loopback (EDGECACHE_ADMIN_ADDR, token required off
# loopback). EDGECACHE_ORIGIN is required at run time.
ENV EDGECACHE_ADDR=0.0.0.0:8081 LOG_FORMAT=json
EXPOSE 8081
USER nonroot:nonroot

ENTRYPOINT ["/edgecache"]
