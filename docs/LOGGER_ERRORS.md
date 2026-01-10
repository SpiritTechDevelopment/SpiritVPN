# Категоризация ошибок в Telegram

## Thread ID топиков

| Топик | Thread ID | Назначение |
|-------|-----------|------------|
| Errors | `13` | Все критические ошибки |
| CI | `18` | Coverage reports |
| Review | `20` | PR notifications |

## Категории ошибок по компонентам

### 1. API Server
**Компонент:** `api-server`
**Префикс:** API

```go
hook := logger.NewTelegramHook(
    os.Getenv("TELEGRAM_BOT_TOKEN"),
    os.Getenv("TELEGRAM_CHAT_ID"),
    "13", // Thread ID: Errors
    "api-server",
)
```

**Типичные ошибки:**
- Ошибки обработки HTTP запросов
- Проблемы с валидацией данных
- Ошибки аутентификации/авторизации
- Проблемы с middleware

### 2. Telegram Bot
**Компонент:** `telegram-bot`
**Префикс:** BOT

```go
hook := logger.NewTelegramHook(
    os.Getenv("TELEGRAM_BOT_TOKEN"),
    os.Getenv("TELEGRAM_CHAT_ID"),
    "13", // Thread ID: Errors
    "telegram-bot",
)
```

**Типичные ошибки:**
- Ошибки Telegram API
- Проблемы с обработкой команд
- Ошибки отправки сообщений

### 3. VPN Server
**Компонент:** `vpn-server`
**Префикс:** VPN

```go
hook := logger.NewTelegramHook(
    os.Getenv("TELEGRAM_BOT_TOKEN"),
    os.Getenv("TELEGRAM_CHAT_ID"),
    "13", // Thread ID: Errors
    "vpn-server",
)
```

**Типичные ошибки:**
- Проблемы с конфигурацией Xray
- Ошибки подключения клиентов
- Проблемы с сетевым интерфейсом

### 4. Infrastructure
**Компонент:** `infrastructure`
**Префикс:** INFRA

```go
hook := logger.NewTelegramHook(
    os.Getenv("TELEGRAM_BOT_TOKEN"),
    os.Getenv("TELEGRAM_CHAT_ID"),
    "13", // Thread ID: Errors
    "infrastructure",
)
```

**Типичные ошибки:**
- Ошибки Redis
- Проблемы с файловой системой
- Сетевые проблемы
- Проблемы с Docker

### 5. Database
**Компонент:** `database`
**Префикс:** DB

```go
hook := logger.NewTelegramHook(
    os.Getenv("TELEGRAM_BOT_TOKEN"),
    os.Getenv("TELEGRAM_CHAT_ID"),
    "13", // Thread ID: Errors
    "database",
)
```

**Типичные ошибки:**
- Ошибки подключения к PostgreSQL
- Проблемы с миграциями
- Медленные запросы
- Deadlocks

## Пример настройки в каждом сервисе

### cmd/api-server/main.go
```go
logConfig := logger.Config{
    Level:         "info",
    ConsoleOutput: true,
    FileOutput:    true,
    FilePath:      "./logs/api-server.log",
}

if err := logger.Setup(&logConfig); err != nil {
    log.Fatalf("Failed to setup logger: %v", err)
}

// Добавление Telegram hook'а для критических ошибок
if botToken := os.Getenv("TELEGRAM_BOT_TOKEN"); botToken != "" {
    hook := logger.NewTelegramHook(
        botToken,
        os.Getenv("TELEGRAM_CHAT_ID"),
        "13", // Errors topic
        "api-server",
    )
    logger.Log.AddHook(hook)
}

log := logger.GetLogger("api-server.main")
log.Info("API Server started")
```

### cmd/telegram-bot/main.go
```go
logConfig := logger.Config{
    Level:         "info",
    ConsoleOutput: true,
    FileOutput:    true,
    FilePath:      "./logs/telegram-bot.log",
}

if err := logger.Setup(&logConfig); err != nil {
    log.Fatalf("Failed to setup logger: %v", err)
}

// Добавление Telegram hook'а
if botToken := os.Getenv("TELEGRAM_BOT_TOKEN"); botToken != "" {
    hook := logger.NewTelegramHook(
        botToken,
        os.Getenv("TELEGRAM_CHAT_ID"),
        "13",
        "telegram-bot",
    )
    logger.Log.AddHook(hook)
}

log := logger.GetLogger("telegram-bot.main")
log.Info("Telegram Bot started")
```

### cmd/vpn-server/main.go
```go
logConfig := logger.Config{
    Level:         "info",
    ConsoleOutput: true,
    FileOutput:    true,
    FilePath:      "./logs/vpn-server.log",
}

if err := logger.Setup(&logConfig); err != nil {
    log.Fatalf("Failed to setup logger: %v", err)
}

// Добавление Telegram hook'а
if botToken := os.Getenv("TELEGRAM_BOT_TOKEN"); botToken != "" {
    hook := logger.NewTelegramHook(
        botToken,
        os.Getenv("TELEGRAM_CHAT_ID"),
        "13",
        "vpn-server",
    )
    logger.Log.AddHook(hook)
}

log := logger.GetLogger("vpn-server.main")
log.Info("VPN Server started")
```

## Формат сообщений в Telegram

Каждое сообщение об ошибке будет выглядеть так:

```
API FATAL

Time: 2026-01-10T15:30:45Z
Module: api.handler

Message:
Database connection failed

Context:
  error: connection refused
  retry_count: 3
  host: postgres:5432
```

## Environment Variables

```bash
# Telegram notifications
TELEGRAM_BOT_TOKEN=your_bot_token
TELEGRAM_CHAT_ID=-1003544840208
```

Не используйте `LOG_TELEGRAM_THREAD_ID` в `.env` - Thread ID задается в коде для каждого компонента.
