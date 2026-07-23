# SpiritVPN

![Go Version](https://img.shields.io/badge/Go-1.24-00ADD8?style=flat&logo=go)
![Status](https://img.shields.io/badge/Status-In_Development-blueviolet)
![License](https://img.shields.io/badge/License-MIT-green)

**SpiritVPN** — backend-сервис на Go для управления VPN-инфраструктурой на базе Xray с поддержкой VLESS + Reality, REST API и внутренней подсистемы учета пользователей, подписок и статистики трафика.

## Назначение

Проект предназначен для построения VPN-сервиса со следующими компонентами:

* **VPN Server** — взаимодействие с Xray и управление VPN-конфигурациями
* **API Server** — REST API для служебных и пользовательских операций
* **Database Layer** — модели, репозитории и работа с PostgreSQL
* **Workers** — фоновые задачи, включая сбор статистики трафика
* **Logger Package** — структурированное логирование с поддержкой файлов, Gin, GORM и Telegram-уведомлений

## Текущее состояние проекта

В текущей версии репозитория доступны:

* базовая структура многокомпонентного сервиса;
* загрузка конфигурации из `configs/.env`;
* API-сервер с health check эндпоинтами;
* слой моделей и репозиториев для PostgreSQL;
* модуль интеграции с Xray;
* worker для сбора статистики трафика;
* пакет структурированного логирования;
* unit-тесты для отдельных пакетов.

Публичные пользовательские сценарии, включая полный billing flow, выдачу клиентских конфигураций через API и завершенные bot/API бизнес-эндпоинты, находятся в стадии развития.

## Архитектура

Структура приложения разделена на отдельные исполняемые компоненты:

1. **VPN Server** — управление Xray и VPN-логикой
2. **API Server** — HTTP API и служебные эндпоинты

Дополнительная информация приведена в `docs/ARCHITECTURE.md`.

## Требования

* Go 1.21+
* PostgreSQL 14+
* Redis 7+
* Xray-core
* Docker и Docker Compose — опционально

## Быстрый старт

### 1. Клонирование репозитория

```bash
git clone https://github.com/RomanRyabinkin/SpiritVPN.git
cd SpiritVPN
```

### 2. Установка зависимостей

```bash
go mod download
```

### 3. Подготовка конфигурации

```bash
cp configs/.env.example configs/.env
```

Заполните переменные окружения в `configs/.env`.

### 4. Подготовка инфраструктуры

Перед запуском сервисов должны быть доступны:

* PostgreSQL
* Redis
* Xray API

### 5. Запуск сервисов

**VPN Server**

```bash
go run cmd/vpn-server/main.go
```

**API Server**

```bash
go run cmd/api-server/main.go
```

### 6. Запуск через Docker Compose

```bash
docker-compose up -d
```

## Конфигурация

Основные настройки приложения задаются в `configs/.env`.

```env
# Database
DB_HOST=localhost
DB_PORT=5432
DB_USER=spiritdb
DB_PASSWORD=your_secure_password_here
DB_NAME=spiritdb

# Redis
REDIS_HOST=localhost
REDIS_PORT=6379
REDIS_PASSWORD=
REDIS_DB=0

# API
API_ADDRESS=:8080
API_MODE=debug
JWT_SECRET=your-jwt-secret-key-change-in-production

# VPN
VPN_HOST=localhost
VPN_PORT=443
VPN_API_PORT=10085
VPN_API_ADDRESS=localhost
VPN_SERVER_NAME=google.com
VPN_PRIVATE_KEY=
VPN_PUBLIC_KEY=
VPN_SHORT_IDS=
VPN_STATS_INTERVAL=5m

# Payment
YOOKASSA_SHOP_ID=your_shop_id
YOOKASSA_SECRET_KEY=your_secret_key

# Logging
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

## Миграции базы данных

В текущей структуре проекта отдельная команда `cmd/migrate` отсутствует.

Для инициализации схемы используется функциональность слоя `internal/database`, включая вызов миграции из кода приложения.

## Структура проекта

```text
SpiritVPN/
├── cmd/                    # Точки входа приложений
│   ├── api-server/         # REST API сервер
│   └── vpn-server/         # VPN сервер
├── configs/                # Конфигурационные файлы
├── deployments/            # Docker-файлы и служебные скрипты запуска
├── docs/                   # Проектная документация
├── examples/               # Примеры использования модулей
├── internal/               # Внутренняя бизнес-логика
│   ├── api/                # API сервер и handlers
│   ├── database/           # Модели, подключение, репозитории
│   ├── vpn/                # Интеграция с Xray и VPN-логика
│   └── workers/            # Фоновые worker'ы
├── pkg/                    # Переиспользуемые пакеты
│   ├── config/             # Загрузка конфигурации
│   └── logger/             # Структурированное логирование
├── test/                   # Тестовые сценарии
│   └── smoke/              # Smoke-сценарии
├── CHANGELOG.md
├── LICENSE
├── LOGGER_IMPLEMENTATION.md
├── Makefile
├── README.md
├── docker-compose.yml
├── go.mod
└── go.sum
```

## Тестирование

### Unit-тесты

```bash
go test ./...
```

### Подробный вывод

```bash
go test -v ./...
```

### Тестирование отдельного пакета

```bash
go test ./pkg/config -v
```

### Покрытие

```bash
make test-coverage
make test-coverage-html
```

### Только unit-тесты

```bash
make test-unit
```

### Smoke-сценарий Xray

В каталоге `test/smoke/` находится отдельный smoke-сценарий для проверки взаимодействия с Xray API.

## Линтинг

```bash
golangci-lint run
```

## Документация

* `docs/API.md` — REST API
* `docs/ARCHITECTURE.md` — архитектура системы
* `docs/DATABASE.md` — модели и слой данных
* `docs/DEPLOYMENT.md` — развертывание
* `docs/FAQ.md` — ответы на частые вопросы
* `docs/ROADMAP.md` — план развития
* `docs/TESTING.md` — тестирование
* `docs/VLESS.md` — описание протокола VLESS и настройки Xray
* `pkg/logger/README.md` — документация по логированию

## Roadmap

К основным направлениям развития проекта относятся:

* развитие пользовательских и административных API;
* интеграция платежных сценариев;
* развитие логики выдачи VPN-конфигураций;
* расширение покрытия тестами;
* развитие многосерверной архитектуры.

## Вклад в проект

1. Создайте fork репозитория
2. Создайте feature-ветку
3. Внесите изменения
4. Добавьте или обновите тесты при необходимости
5. Откройте Pull Request

## Лицензия

Проект распространяется под лицензией MIT. Подробности приведены в файле `LICENSE`.

## Авторы

* **Roman Ryabinkin** — @RomanRyabinkin
* **Pavel Lensky** — @xvpaul
