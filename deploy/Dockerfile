# ---- build ----
# --platform=$BUILDPLATFORM keeps the compiler on the runner's native arch and
# cross-compiles via GOARCH — CGO is off, so Go does this for free and a two-arch
# manifest costs a second `go build`, not a QEMU-emulated toolchain.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG TARGETARCH
# Cache mounts survive across builds on one builder (local dev, a persistent CI
# builder); on ephemeral runners the registry layer cache does the same job.
# -trimpath strips the build host's paths; -s -w drops the symbol/DWARF tables.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/bank0 ./cmd/app

# ---- run ----
# :nonroot runs as uid 65532, so the image is non-root by default — before the
# Kubernetes securityContext (deploy/helm) even pins runAsNonRoot. The binary +
# config are world-readable/executable, so uid 65532 can read config.yaml/openapi
# and exec the binary on a read-only root filesystem (the app writes nothing).
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/bank0 /app/bank0
COPY config.yaml /app/config.yaml
COPY api/openapi.yaml /app/api/openapi.yaml
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/app/bank0"]
CMD ["serve"]
