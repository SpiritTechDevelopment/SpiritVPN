# 🏗️ Архитектура SpiritVPN

## Обзор системы

SpiritVPN построен на микросервисной архитектуре с тремя основными компонентами, взаимодействующими через REST API и общую базу данных.

```
┌─────────────────┐
│   Telegram Bot  │
│                 │
│  - Регистрация  │
│  - Платежи      │
│  - Выдача       │
│    конфигов     │
└────────┬────────┘
         │
         │ REST API
         │
         ▼
┌─────────────────┐      ┌──────────────┐      ┌─────────────┐
│   API Server    │◄────►│  PostgreSQL  │◄────►│    Redis    │
│                 │      │              │      │   (Cache)   │
│  - Auth         │      │  - Users     │      └─────────────┘
│  - User Mgmt    │      │  - Subs      │
│  - Config Gen   │      │  - Configs   │
│  - Billing      │      │  - Payments  │
└────────┬────────┘      └──────────────┘
         │
         │ gRPC/API
         │
         ▼
┌─────────────────┐      ┌──────────────┐
│   VPN Server    │◄────►│  WireGuard   │
│                 │      │   Kernel     │
│  - Routing      │      │   Module     │
│  - Traffic      │      └──────────────┘
│  - Monitoring   │
└─────────────────┘
```

## Компоненты системы

### 1. VPN Server

**Технологии:**
- WireGuard (протокол VPN)
- Go (управляющий сервис)
- iptables (маршрутизация)

**Функции:**
- Прием и обработка VPN соединений
- Маршрутизация трафика пользователей
- Сбор статистики по использованию
- Управление пирами (peers)
- Мониторинг состояния соединений

**Файловая структура:**
```
internal/vpn/
├── server.go          # Основной VPN сервер
├── wireguard.go       # Интеграция с WireGuard
├── peer.go            # Управление пирами
├── traffic.go         # Учет трафика
└── monitor.go         # Мониторинг соединений
```

### 2. API Server

**Технологии:**
- Gin (web framework)
- GORM (ORM)
- JWT (аутентификация)

**Функции:**
- REST API для управления системой
- Аутентификация и авторизация
- CRUD операции для пользователей
- Управление подписками
- Генерация конфигураций
- Биллинг и платежи
- Статистика и аналитика

**Эндпоинты:**
```
POST   /api/v1/auth/register
POST   /api/v1/auth/login
GET    /api/v1/users/:id
POST   /api/v1/subscriptions
GET    /api/v1/configs/:user_id
POST   /api/v1/payments
GET    /api/v1/stats/:user_id
```

**Файловая структура:**
```
internal/api/
├── handlers/
│   ├── auth.go        # Аутентификация
│   ├── users.go       # Пользователи
│   ├── subscriptions.go
│   ├── configs.go
│   └── payments.go
├── middleware/
│   ├── auth.go        # JWT middleware
│   └── cors.go
├── routes/
│   └── routes.go      # Роуты
└── server.go          # HTTP сервер
```

### 3. Telegram Bot

**Технологии:**
- go-telegram-bot-api
- Inline клавиатуры
- Webhook/Long Polling

**Функции:**
- Регистрация новых пользователей
- Выбор и покупка подписок
- Интеграция с платежными системами
- Выдача конфигурационных файлов
- Продление подписок
- Статистика использования
- Поддержка пользователей

**Сценарии использования:**
```
/start          → Регистрация/Главное меню
/buy            → Выбор тарифа → Оплата → Конфиг
/myconfig       → Получение конфига
/stats          → Статистика использования
/extend         → Продление подписки
/support        → Связь с поддержкой
```

**Файловая структура:**
```
internal/bot/
├── handlers/
│   ├── start.go       # Команда /start
│   ├── buy.go         # Покупка подписки
│   ├── config.go      # Выдача конфигов
│   └── stats.go       # Статистика
├── keyboards/
│   └── inline.go      # Inline клавиатуры
├── middleware/
│   └── auth.go        # Проверка пользователя
└── bot.go             # Основной бот
```

## База данных

### PostgreSQL Schema

```sql
-- Пользователи
CREATE TABLE users (
    id SERIAL PRIMARY KEY,
    telegram_id BIGINT UNIQUE NOT NULL,
    username VARCHAR(255),
    email VARCHAR(255),
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW()
);

-- Подписки
CREATE TABLE subscriptions (
    id SERIAL PRIMARY KEY,
    user_id INTEGER REFERENCES users(id),
    plan_type VARCHAR(50) NOT NULL,
    start_date TIMESTAMP NOT NULL,
    end_date TIMESTAMP NOT NULL,
    is_active BOOLEAN DEFAULT true,
    auto_renew BOOLEAN DEFAULT false,
    created_at TIMESTAMP DEFAULT NOW()
);

-- Конфигурации VPN
CREATE TABLE vpn_configs (
    id SERIAL PRIMARY KEY,
    user_id INTEGER REFERENCES users(id),
    subscription_id INTEGER REFERENCES subscriptions(id),
    public_key TEXT NOT NULL,
    private_key TEXT NOT NULL,
    ip_address INET NOT NULL,
    server_id INTEGER REFERENCES vpn_servers(id),
    created_at TIMESTAMP DEFAULT NOW()
);

-- VPN серверы
CREATE TABLE vpn_servers (
    id SERIAL PRIMARY KEY,
    name VARCHAR(255) NOT NULL,
    host VARCHAR(255) NOT NULL,
    port INTEGER NOT NULL,
    public_key TEXT NOT NULL,
    location VARCHAR(100),
    is_active BOOLEAN DEFAULT true,
    max_users INTEGER DEFAULT 1000,
    current_users INTEGER DEFAULT 0
);

-- Платежи
CREATE TABLE payments (
    id SERIAL PRIMARY KEY,
    user_id INTEGER REFERENCES users(id),
    subscription_id INTEGER REFERENCES subscriptions(id),
    amount DECIMAL(10, 2) NOT NULL,
    currency VARCHAR(3) DEFAULT 'RUB',
    status VARCHAR(50) NOT NULL,
    payment_method VARCHAR(50),
    transaction_id VARCHAR(255),
    created_at TIMESTAMP DEFAULT NOW()
);

-- Статистика трафика
CREATE TABLE traffic_stats (
    id SERIAL PRIMARY KEY,
    user_id INTEGER REFERENCES users(id),
    config_id INTEGER REFERENCES vpn_configs(id),
    bytes_sent BIGINT DEFAULT 0,
    bytes_received BIGINT DEFAULT 0,
    date DATE NOT NULL,
    created_at TIMESTAMP DEFAULT NOW()
);
```

### Redis (Кеширование)

```
Ключи:
- user:{telegram_id}           # Данные пользователя
- config:{user_id}             # VPN конфигурация
- stats:{user_id}:{date}       # Статистика за день
- session:{token}              # JWT сессии
- rate_limit:{telegram_id}     # Rate limiting
```

## Безопасность

### Аутентификация

1. **Telegram Bot**: Аутентификация через Telegram ID
2. **API**: JWT токены с refresh механизмом
3. **VPN**: Аутентификация через WireGuard ключи

### Шифрование

- WireGuard: Использует ChaCha20 для шифрования
- API: HTTPS/TLS 1.3
- БД: Шифрование чувствительных данных (AES-256)

### Защита от атак

- Rate limiting (Redis)
- CORS настройки
- SQL injection защита (GORM prepared statements)
- XSS защита
- DDoS защита на уровне сервера

## Масштабирование

### Горизонтальное масштабирование

```
                    ┌─────────────┐
                    │   Nginx LB  │
                    └──────┬──────┘
                           │
            ┌──────────────┼──────────────┐
            │              │              │
       ┌────▼────┐    ┌────▼────┐   ┌────▼────┐
       │  API 1  │    │  API 2  │   │  API 3  │
       └─────────┘    └─────────┘   └─────────┘
            │              │              │
            └──────────────┼──────────────┘
                           │
                    ┌──────▼──────┐
                    │ PostgreSQL  │
                    │   Master    │
                    └──────┬──────┘
                           │
                    ┌──────┴──────┐
                    │             │
              ┌─────▼────┐  ┌─────▼────┐
              │ Replica1 │  │ Replica2 │
              └──────────┘  └──────────┘
```

### Мультисерверная архитектура VPN

```
┌──────────────┐
│  API Server  │
└──────┬───────┘
       │
       │ Управление
       │
   ┌───┴───────────┬────────────┬────────────┐
   │               │            │            │
┌──▼───────┐  ┌───▼────────┐  ┌▼────────┐  ┌▼────────┐
│ VPN US-1 │  │ VPN EU-1   │  │ VPN AS-1│  │ VPN RU-1│
│ (USA)    │  │ (Germany)  │  │ (Japan) │  │ (Russia)│
└──────────┘  └────────────┘  └─────────┘  └─────────┘
```

## Мониторинг и Логирование

### Метрики (Prometheus)

```
- vpn_active_connections
- vpn_traffic_bytes_total
- api_requests_total
- api_response_time
- payment_success_rate
- subscription_active_count
```

### Логирование (Structured logging)

```go
logger.Info("User connected",
    "user_id", userID,
    "server", serverName,
    "ip", clientIP,
)
```

### Алерты

- Падение VPN сервера
- Превышение лимита подключений
- Ошибки платежей > 5%
- Проблемы с БД
- Высокая нагрузка на CPU/RAM

## CI/CD Pipeline

```
┌──────────┐
│   Git    │
│  Push    │
└────┬─────┘
     │
     ▼
┌──────────────┐
│ GitHub       │
│ Actions      │
└────┬─────────┘
     │
     ├──► Tests (unit, integration)
     ├──► Linting
     ├──► Build Docker images
     │
     ▼
┌──────────────┐
│ Docker       │
│ Registry     │
└────┬─────────┘
     │
     ▼
┌──────────────┐
│ Deploy to    │
│ Production   │
└──────────────┘
```

## Развертывание

### Production Stack

```yaml
# docker-compose.yml
services:
  postgres:
    image: postgres:14
  redis:
    image: redis:7
  api:
    build: ./cmd/api-server
    replicas: 3
  bot:
    build: ./cmd/telegram-bot
  vpn:
    build: ./cmd/vpn-server
    cap_add:
      - NET_ADMIN
  nginx:
    image: nginx:latest
  prometheus:
    image: prom/prometheus
  grafana:
    image: grafana/grafana
```

---

Эта архитектура обеспечивает:
- ✅ Высокую доступность
- ✅ Масштабируемость
- ✅ Безопасность
- ✅ Простоту поддержки
- ✅ Мониторинг и логирование
