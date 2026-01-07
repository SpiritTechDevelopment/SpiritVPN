# VPN Server Dockerfile
FROM golang:1.21-alpine AS builder

WORKDIR /app


RUN apk add --no-cache git linux-headers


COPY go.mod go.sum ./
RUN go mod download

COPY . .


RUN CGO_ENABLED=0 GOOS=linux go build -o vpn-server ./cmd/vpn-server

FROM alpine:latest


COPY --from=ghcr.io/xtls/xray-core:latest /usr/bin/xray /usr/bin/xray
COPY --from=ghcr.io/xtls/xray-core:latest /usr/share/xray /usr/share/xray

WORKDIR /root/

COPY --from=builder /app/vpn-server .

COPY configs/xray.json /etc/xray/xray.json
COPY deployments/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 443/tcp

ENTRYPOINT ["/entrypoint.sh"]
