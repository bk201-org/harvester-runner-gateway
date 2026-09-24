# syntax=docker/dockerfile:1
FROM golang:1.26.8-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/harvester-runner-gateway ./cmd/harvester-runner-gateway

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/harvester-runner-gateway /usr/local/bin/harvester-runner-gateway
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/harvester-runner-gateway"]
