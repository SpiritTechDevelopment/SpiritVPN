FROM golang:1.23-alpine AS builder

WORKDIR /app

ENV GOTOOLCHAIN=auto

RUN apk add --no-cache git linux-headers ca-certificates


COPY go.mod go.sum ./
RUN GOTOOLCHAIN=auto go mod download

COPY . .


RUN CGO_ENABLED=0 GOOS=linux GOTOOLCHAIN=auto go build -o vpn-server ./cmd/vpn-server

FROM alpine:latest

COPY --from=ghcr.io/xtls/xray-core:latest /usr/local/bin/xray /usr/bin/xray

WORKDIR /root/

COPY --from=builder /app/vpn-server .

COPY configs/xray.json /etc/xray/xray.json
COPY deployments/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 443/tcp

ENTRYPOINT ["/entrypoint.sh"]
