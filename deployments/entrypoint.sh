#!/bin/sh
set -e

if [ -f /etc/xray/xray.json ]; then
  echo "Starting xray with /etc/xray/xray.json"
  /usr/bin/xray -c /etc/xray/xray.json &
else
  echo "Warning: /etc/xray/xray.json not found, starting xray with default config"
  /usr/bin/xray &
fi

# Ожидание инициализации xray
echo "Waiting for xray to initialize..."
sleep 1

# Запуск VPN сервера
exec /root/vpn-server
