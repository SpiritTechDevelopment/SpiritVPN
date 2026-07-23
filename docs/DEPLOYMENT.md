# Deployment Guide

## Назначение

Документ описывает базовые требования и порядок развертывания компонентов SpiritVPN в текущем состоянии репозитория.

## Компоненты развертывания

В проект входят следующие исполняемые сервисы:

* `api-server`
* `vpn-server`

Для контейнерного запуска в репозитории предусмотрены:

* `deployments/api.Dockerfile`
* `deployments/vpn.Dockerfile`
* `deployments/entrypoint.sh`
* `docker-compose.yml`

## Требования окружения

Для запуска необходимы:

* Go 1.21+;
* PostgreSQL 14+;
* Redis 7+;
* Xray-core;
* Docker и Docker Compose — при контейнерном запуске.

## Подготовка конфигурации

Создайте локальный конфигурационный файл:

```bash
cp configs/.env.example configs/.env
```

Заполните как минимум следующие группы параметров:

* параметры подключения к PostgreSQL;
* параметры подключения к Redis;
* параметры VPN/Xray;
* параметры логирования.

## Локальный запуск

### API Server

```bash
go run cmd/api-server/main.go
```

### VPN Server

```bash
go run cmd/vpn-server/main.go
```

## Развертывание через Docker Compose

Для базового контейнерного запуска используйте:

```bash
docker-compose up -d
```

Для просмотра состояния контейнеров:

```bash
docker-compose ps
```

Для просмотра логов:

```bash
docker-compose logs -f
```

## Переменные окружения

Ниже приведен пример основных переменных:

```env
DB_HOST=localhost
DB_PORT=5432
DB_USER=spiritdb
DB_PASSWORD=your_secure_password_here
DB_NAME=spiritdb

REDIS_HOST=localhost
REDIS_PORT=6379
REDIS_PASSWORD=
REDIS_DB=0

API_ADDRESS=:8080
API_MODE=debug
JWT_SECRET=your-jwt-secret-key-change-in-production

VPN_HOST=localhost
VPN_PORT=443
VPN_API_PORT=10085
VPN_API_ADDRESS=localhost
VPN_SERVER_NAME=google.com
VPN_PRIVATE_KEY=
VPN_PUBLIC_KEY=
VPN_SHORT_IDS=
VPN_STATS_INTERVAL=5m

YOOKASSA_SHOP_ID=your_shop_id
YOOKASSA_SECRET_KEY=your_secret_key

LOG_LEVEL=info
LOG_DIR=./logs
LOG_CONSOLE=true
LOG_FILE=true
LOG_COLORED=true
LOG_ERROR_FILE=true
LOG_ENABLED=true
LOG_MAX_FILE_SIZE=10
LOG_MAX_BACKUPS=5
LOG_MAX_AGE=30
LOG_TELEGRAM_BOT_TOKEN=
LOG_TELEGRAM_CHAT_ID=
LOG_TELEGRAM_THREAD_ID=
```

## Подготовка Xray

Для корректной работы VPN-компонента должны быть подготовлены:

* установленный Xray-core;
* доступный Xray API;
* согласованная конфигурация `configs/xray.json`;
* корректные Reality-ключи и значения `VPN_PRIVATE_KEY`, `VPN_PUBLIC_KEY`, `VPN_SHORT_IDS`.

## База данных

В текущей структуре проекта отдельная CLI-команда миграций не выделена. Подготовка схемы выполняется через слой `internal/database` из логики приложения.

Перед запуском рекомендуется убедиться, что:

* PostgreSQL доступен;
* учетные данные корректны;
* база данных создана;
* приложение имеет права на подключение и изменение схемы.

## Проверка запуска

После старта API-сервера доступны базовые health check эндпоинты:

* `GET /health`
* `GET /health/advanced`

Пример проверки:

```bash
curl http://localhost:8080/health
```

## Логирование

Для сервисов можно использовать файловое и консольное логирование через `pkg/logger`.

При включенном файловом логировании создаются:

```text
logs/
├── spirit_vpn.log
└── spirit_vpn_error.log
```

## Рекомендации по production-развертыванию

1. Хранить `.env` вне репозитория
2. Не сохранять приватные ключи и токены в Git
3. Включать файловое логирование и ротацию логов
4. Использовать отдельные учетные записи для БД и Redis
5. Проверять доступность внешних портов и внутренних сервисов
6. Поддерживать согласованность `configs/.env` и `configs/xray.json`

## Диагностика проблем

### API не отвечает

Проверьте:

* корректность `API_ADDRESS`;
* логи API-сервера;
* доступность порта;
* подключение к базе данных.

### VPN Server не работает корректно

Проверьте:

* доступность Xray API;
* корректность `configs/xray.json`;
* значения `VPN_API_ADDRESS`, `VPN_API_PORT`, `VPN_SERVER_NAME`;
* валидность Reality-ключей.

### Ошибки подключения к PostgreSQL

Проверьте:

* `DB_HOST`, `DB_PORT`, `DB_USER`, `DB_PASSWORD`, `DB_NAME`;
* наличие базы данных;
* права пользователя.

## Связанные документы

* `README.md`
* `docs/ARCHITECTURE.md`
* `docs/API.md`
* `docs/DATABASE.md`
* `docs/VLESS.md`
* `pkg/logger/README.md`
