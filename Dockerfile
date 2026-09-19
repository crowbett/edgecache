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
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/api ./cmd/edgecache

# ---------- runtime ----------
# distroless/static: no shell, no package manager, runs as uid 65532 (nonroot).
# NOTE: HEALTHCHECK is intentionally absent -- distroless has no shell/curl/wget to run it
# and Kubernetes/compose should probe GET /healthz and /readyz over HTTP instead.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/api /api

ENV HTTP_ADDR=:8080 LOG_FORMAT=json
EXPOSE 8080
USER nonroot:nonroot

ENTRYPOINT ["/api"]
