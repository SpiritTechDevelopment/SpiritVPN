# Гайд по деплою SpiritVPN

## Подготовка сервера

### Требования

- Ubuntu 20.04+ / Debian 11+
- Минимум 2 CPU cores
- Минимум 2GB RAM
- 20GB SSD
- Публичный IP адрес
- Root доступ

### Установка необходимого ПО

```bash
# Обновление системы
sudo apt update && sudo apt upgrade -y

# Установка Docker
curl -fsSL https://get.docker.com -o get-docker.sh
sudo sh get-docker.sh

# Установка Docker Compose
sudo apt install docker-compose -y

# Установка WireGuard (для VPN сервера)
sudo apt install wireguard -y

# Разрешение IP forwarding
echo "net.ipv4.ip_forward=1" | sudo tee -a /etc/sysctl.conf
sudo sysctl -p
```

## Деплой через Docker Compose

### 1. Клонирование репозитория

```bash
git clone https://github.com/RomanRyabinkin/SpiritVPN.git
cd SpiritVPN
```

### 2. Настройка переменных окружения

```bash
cp configs/.env.example configs/.env
nano configs/.env
```

Заполните все необходимые переменные:
- `TELEGRAM_BOT_TOKEN` - токен от BotFather
- `DB_PASSWORD` - надежный пароль для PostgreSQL
- `JWT_SECRET` - секретный ключ для JWT
- `YOOKASSA_*` - данные от платежной системы

### 3. Генерация WireGuard ключей

```bash
wg genkey | tee privatekey | wg pubkey > publickey
cat privatekey  # Скопировать в VPN_PRIVATE_KEY
```

### 4. Запуск сервисов

```bash
docker-compose up -d
```

### 5. Проверка статуса

```bash
docker-compose ps
docker-compose logs -f
```

## Деплой вручную (без Docker)

### 1. Установка Go

```bash
wget https://go.dev/dl/go1.21.0.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go1.21.0.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc
```

### 2. Установка PostgreSQL

```bash
sudo apt install postgresql postgresql-contrib -y
sudo -u postgres psql

CREATE DATABASE spiritdb;
CREATE USER spiritdb WITH PASSWORD 'your_password';
GRANT ALL PRIVILEGES ON DATABASE spiritdb TO spiritdb;
\q
```

### 3. Установка Redis

```bash
sudo apt install redis-server -y
sudo systemctl enable redis-server
sudo systemctl start redis-server
```

### 4. Сборка проекта

```bash
cd SpiritVPN
go mod download
make build
```

### 5. Создание systemd сервисов

**API Server** (`/etc/systemd/system/spiritvpn-api.service`):
```ini
[Unit]
Description=SpiritVPN API Server
After=network.target postgresql.service

[Service]
Type=simple
User=root
WorkingDirectory=/opt/SpiritVPN
EnvironmentFile=/opt/SpiritVPN/configs/.env
ExecStart=/opt/SpiritVPN/bin/api-server
Restart=always

[Install]
WantedBy=multi-user.target
```

**VPN Server** (`/etc/systemd/system/spiritvpn-vpn.service`):
```ini
[Unit]
Description=SpiritVPN VPN Server
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/opt/SpiritVPN
EnvironmentFile=/opt/SpiritVPN/configs/.env
ExecStart=/opt/SpiritVPN/bin/vpn-server
Restart=always

[Install]
WantedBy=multi-user.target
```

**Telegram Bot** (`/etc/systemd/system/spiritvpn-bot.service`):
```ini
[Unit]
Description=SpiritVPN Telegram Bot
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/opt/SpiritVPN
EnvironmentFile=/opt/SpiritVPN/configs/.env
ExecStart=/opt/SpiritVPN/bin/telegram-bot
Restart=always

[Install]
WantedBy=multi-user.target
```

### 6. Запуск сервисов

```bash
sudo systemctl daemon-reload
sudo systemctl enable spiritvpn-api spiritvpn-vpn spiritvpn-bot
sudo systemctl start spiritvpn-api spiritvpn-vpn spiritvpn-bot
```

## Настройка Nginx

### Установка

```bash
sudo apt install nginx certbot python3-certbot-nginx -y
```

### Конфигурация

`/etc/nginx/sites-available/spiritvpn`:

```nginx
server {
    listen 80;
    server_name api.spiritvpn.com;

    location / {
        proxy_pass http://localhost:8080;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection 'upgrade';
        proxy_set_header Host $host;
        proxy_cache_bypass $http_upgrade;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    }
}
```

### Активация и SSL

```bash
sudo ln -s /etc/nginx/sites-available/spiritvpn /etc/nginx/sites-enabled/
sudo nginx -t
sudo systemctl reload nginx

# Получение SSL сертификата
sudo certbot --nginx -d api.spiritvpn.com
```

## Настройка файрвола

```bash
# Разрешить необходимые порты
sudo ufw allow 22/tcp      # SSH
sudo ufw allow 80/tcp      # HTTP
sudo ufw allow 443/tcp     # HTTPS
sudo ufw allow 51820/udp   # WireGuard
sudo ufw enable
```

## Мониторинг

### Prometheus + Grafana

После запуска через docker-compose:

- Prometheus: `http://your-server:9090`
- Grafana: `http://your-server:3000`
  - Login: admin
  - Password: admin (или из переменной)

### Настройка алертов

Создайте файл `configs/alertmanager.yml`:

```yaml
global:
  resolve_timeout: 5m

route:
  receiver: 'telegram'
  
receivers:
  - name: 'telegram'
    telegram_configs:
      - bot_token: 'YOUR_BOT_TOKEN'
        chat_id: YOUR_CHAT_ID
        message: '{{ range .Alerts }}{{ .Annotations.summary }}{{ end }}'
```

## Backup

### Автоматический backup базы данных

Создайте скрипт `/opt/backup.sh`:

```bash
#!/bin/bash
BACKUP_DIR="/opt/backups"
DATE=$(date +%Y%m%d_%H%M%S)
PGPASSWORD="your_password" pg_dump -U spiritdb -h localhost spiritdb > $BACKUP_DIR/db_$DATE.sql
find $BACKUP_DIR -mtime +7 -delete
```

Добавьте в crontab:
```bash
crontab -e
0 2 * * * /opt/backup.sh
```

## Обновление

### С использованием Docker

```bash
cd SpiritVPN
git pull
docker-compose down
docker-compose build
docker-compose up -d
```

### Без Docker

```bash
cd SpiritVPN
git pull
make build
sudo systemctl restart spiritvpn-api spiritvpn-vpn spiritvpn-bot
```

## Troubleshooting

### Проверка логов

**Docker:**
```bash
docker-compose logs api
docker-compose logs vpn
docker-compose logs bot
```

**Systemd:**
```bash
sudo journalctl -u spiritvpn-api -f
sudo journalctl -u spiritvpn-vpn -f
sudo journalctl -u spiritvpn-bot -f
```

### Проверка подключения к БД

```bash
docker-compose exec postgres psql -U spiritdb -d spiritdb
```

### Проверка WireGuard

```bash
sudo wg show
```

### Перезапуск сервисов

```bash
docker-compose restart api
# или
sudo systemctl restart spiritvpn-api
```

## Масштабирование

### Добавление нового VPN сервера

1. Разверните сервер в новой локации
2. Установите VPN компонент
3. Зарегистрируйте сервер в БД
4. Настройте балансировку

### Горизонтальное масштабирование API

Используйте Kubernetes или Docker Swarm для запуска множественных экземпляров API сервера за load balancer'ом.

---

## Чеклист перед продакшеном

- [ ] Все пароли изменены на безопасные
- [ ] SSL сертификаты установлены
- [ ] Файрвол настроен
- [ ] Backup настроен
- [ ] Мониторинг работает
- [ ] Логи ротируются
- [ ] Тестовые платежи проходят
- [ ] DNS записи настроены
- [ ] Документация актуальна
