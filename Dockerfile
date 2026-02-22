# Dockerfile for the scalerunnergh orchestrator.
# Multi-stage build: compiles the Go binary, then copies into a minimal image.
FROM golang:1.23-alpine AS builder

WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download || true
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags "-s -w" -o /scalerunnergh ./cmd/scalerunnergh

FROM alpine:3.19

RUN apk --no-cache add ca-certificates docker-cli

COPY --from=builder /scalerunnergh /usr/local/bin/scalerunnergh

ENTRYPOINT ["scalerunnergh"]
