# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS builder

WORKDIR /app

RUN apk add --no-cache ca-certificates git

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/api-server ./cmd/api-server

FROM alpine:3.22

RUN apk add --no-cache ca-certificates && \
    addgroup -S -g 10001 spiritvpn && \
    adduser -S -D -H -u 10001 -G spiritvpn spiritvpn

WORKDIR /app

COPY --from=builder --chown=spiritvpn:spiritvpn /out/api-server /app/api-server

USER 10001:10001

ENV LOG_CONSOLE=true \
    LOG_FILE=false \
    LOG_ERROR_FILE=false

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget --quiet --tries=1 --spider http://127.0.0.1:8080/health || exit 1

ENTRYPOINT ["/app/api-server"]
