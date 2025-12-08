# VPN Server Dockerfile
FROM golang:1.21-alpine AS builder

WORKDIR /app

# Install dependencies
RUN apk add --no-cache git linux-headers

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build
RUN CGO_ENABLED=0 GOOS=linux go build -o vpn-server ./cmd/vpn-server

# Final stage
FROM alpine:latest

# Устанавливаем Xray вместо WireGuard
COPY --from=ghcr.io/xtls/xray-core:latest /usr/bin/xray /usr/bin/xray
COPY --from=ghcr.io/xtls/xray-core:latest /usr/share/xray /usr/share/xray

WORKDIR /root/

COPY --from=builder /app/vpn-server .

# VLESS обычно использует 443 порт
EXPOSE 443/tcp 

CMD ["./vpn-server"]
