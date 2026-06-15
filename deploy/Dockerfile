# ---- build ----
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/bank0 ./cmd/app

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
