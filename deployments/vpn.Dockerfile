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

RUN apk --no-cache add \
    ca-certificates \
    iptables \
    wireguard-tools

WORKDIR /root/

COPY --from=builder /app/vpn-server .

EXPOSE 51820/udp

CMD ["./vpn-server"]
