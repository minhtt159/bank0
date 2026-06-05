# ---- build ----
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/bank0 ./cmd/app

# ---- run ----
FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=build /out/bank0 /app/bank0
COPY config.yaml /app/config.yaml
COPY api/openapi.yaml /app/api/openapi.yaml
EXPOSE 8080
ENTRYPOINT ["/app/bank0"]
CMD ["serve"]
