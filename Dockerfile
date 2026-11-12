# Orchestrator image. The docker CLI is included because the docker backend
# drives the host daemon through the mounted socket; job containers never get it.
FROM golang:1.27-alpine AS builder

ARG VERSION=dev
ARG BUILD_TIME=unknown

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.buildTime=${BUILD_TIME}" \
    -o /scalerunnergh ./cmd/scalerunnergh

FROM alpine:3.22

RUN apk --no-cache add ca-certificates docker-cli tini

COPY --from=builder /scalerunnergh /usr/local/bin/scalerunnergh

EXPOSE 9090
ENTRYPOINT ["tini", "--", "scalerunnergh"]
CMD ["--config", "/etc/scalerunnergh/config.json"]
