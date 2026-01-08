# SpiritVPN

**SpiritVPN** - VPN сервис с продажей подписок через Telegram бота, написанный на Go.

## Описание

SpiritVPN представляет собой комплексное решение для создания и управления VPN-сервисом:
- VPN-сервер на базе Xray с поддержкой VLESS+Reality протокола
- Telegram бот для продажи подписок и управления аккаунтом
- Интеграция с платежными системами
- API для управления пользователями и серверами
- Административная панель

## Особенности

- VPN сервер на базе VLESS протокола с поддержкой Reality
- Высокая производительность и защита от обнаружения
- Telegram бот для управления подписками
- REST API для интеграции
- Интеграция с ЮKassa для приема платежей
- PostgreSQL + Redis для хранения данных
- Docker-ready архитектура
- QR-коды для быстрой настройки клиентов

## Архитектура

Проект состоит из трех основных компонентов:

1. **VPN Server** - сервер для маршрутизации трафика пользователей
2. **API Server** - REST API для управления системой
3. **Telegram Bot** - интерфейс для пользователей

Подробнее в [ARCHITECTURE.md](docs/ARCHITECTURE.md)

## Быстрый старт

### Требования

- Go 1.21+
- PostgreSQL 14+
- Redis 7+
- Docker & Docker Compose (опционально)

### Установка

1. Клонируйте репозиторий:
```bash
git clone https://github.com/RomanRyabinkin/SpiritVPN.git
cd SpiritVPN
```

2. Установите зависимости:
```bash
go mod download
```

3. Создайте файл конфигурации:
```bash
cp configs/.env.example configs/.env
```

4. Настройте переменные окружения в `configs/.env`

5. Запустите миграции базы данных:
```bash
go run cmd/migrate/main.go
```

6. Запустите сервисы:

**VPN Server:**
```bash
go run cmd/vpn-server/main.go
```

**API Server:**
```bash
go run cmd/api-server/main.go
```

**Telegram Bot:**
```bash
go run cmd/telegram-bot/main.go
```

### Docker

Запуск всех сервисов через Docker Compose:

```bash
docker-compose up -d
```

## Структура проекта

```
SpiritVPN/
├── cmd/                    # Точки входа приложений
│   ├── vpn-server/        # VPN сервер
│   ├── api-server/        # REST API сервер
│   └── telegram-bot/      # Telegram бот
├── internal/              # Внутренняя бизнес-логика
│   ├── vpn/              # Логика VPN сервера
│   ├── api/              # API handlers и routes
│   ├── bot/              # Telegram бот handlers
│   ├── database/         # Работа с БД
│   └── payment/          # Платежные системы
├── pkg/                   # Публичные библиотеки
│   └── config/           # Конфигурация
├── docs/                  # Документация
├── configs/              # Конфигурационные файлы
├── deployments/          # Docker, Kubernetes конфиги
└── scripts/              # Вспомогательные скрипты
```

## Документация

- [Архитектура системы](docs/ARCHITECTURE.md)
- [API документация](docs/API.md)
- [План разработки](docs/ROADMAP.md)
- [Гайд по деплою](docs/DEPLOYMENT.md)
- [Руководство по тестированию](docs/TESTING.md)
- [FAQ](docs/FAQ.md)

## Конфигурация

Основные настройки в файле `configs/.env`:

```env
# Database
DB_HOST=localhost
DB_PORT=5432
DB_USER=spiritdb
DB_PASSWORD=your_password
DB_NAME=spiritdb

# Redis
REDIS_HOST=localhost
REDIS_PORT=6379

# Telegram Bot
TELEGRAM_BOT_TOKEN=your_bot_token

# Payment
YOOKASSA_SHOP_ID=your_shop_id
YOOKASSA_SECRET_KEY=your_secret_key

# VPN
VPN_SERVER_PORT=443
VPN_SERVER_NAME=google.com
VPN_SHORT_IDS=
VPN_PRIVATE_KEY=your_x25519_private_key
```

## Разработка

### Запуск тестов

```bash
# Unit-тесты
go test ./...

# Smoke-тесты
docker compose up -d vpn
go run test/smoke/xray_test.go
```

Подробнее в [TESTING.md](docs/TESTING.md)

### Запуск с hot-reload

```bash
# Установите air
go install github.com/cosmtrek/air@latest

# Запустите
air
```

### Линтинг

```bash
golangci-lint run
```

## Roadmap

- [x] Инициализация проекта
- [ ] Реализация VPN сервера (VLESS)
- [ ] REST API для управления
- [ ] Telegram бот
- [ ] Интеграция платежей
- [ ] Админ панель
- [ ] Мультисерверная архитектура
- [ ] Мониторинг и логирование
- [ ] CI/CD pipeline

Полный план в [ROADMAP.md](docs/ROADMAP.md)

## Вклад в проект

Мы приветствуем любой вклад! Пожалуйста:

1. Форкните репозиторий
2. Создайте feature-ветку (`git checkout -b feature/AmazingFeature`)
3. Закоммитьте изменения (`git commit -m 'Add some AmazingFeature'`)
4. Запушьте в ветку (`git push origin feature/AmazingFeature`)
5. Откройте Pull Request

## Лицензия

Этот проект распространяется под лицензией MIT. Подробности в файле [LICENSE](LICENSE).

## Авторы

**Roman Ryabinkin**
- GitHub: [@RomanRyabinkin](https://github.com/RomanRyabinkin)
**Pavel Lensky**
- GitHub: [@xvpaul](https://github.com/xvpaul)


## Благодарности

- Сообщество Go за саппорт

## Поддержка

Если у вас есть вопросы или проблемы:
- Создайте [Issue](https://github.com/RomanRyabinkin/SpiritVPN/issues)
- Напишите в [Discussions](https://github.com/RomanRyabinkin/SpiritVPN/discussions)

---
