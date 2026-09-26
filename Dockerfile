# syntax=docker/dockerfile:1
FROM golang:1.26.8-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/hvst-runner-gw ./cmd/hvst-runner-gw

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/hvst-runner-gw /usr/local/bin/hvst-runner-gw
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/hvst-runner-gw"]
