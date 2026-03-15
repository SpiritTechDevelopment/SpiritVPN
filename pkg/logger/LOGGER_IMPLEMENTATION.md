# Logger Implementation

## Статус

В проекте реализован отдельный пакет структурированного логирования `pkg/logger`, предназначенный для использования во всех основных компонентах SpiritVPN.

## Назначение пакета

Пакет логирования решает следующие задачи:

* единый формат логов для всех сервисов;
* консольное и файловое логирование;
* ротация файлов логов;
* выделение ошибок в отдельный файл;
* контекстное логирование;
* интеграция с Gin и GORM;
* отправка критических уведомлений в Telegram.

## Состав реализации

### `pkg/logger/logger.go`

Содержит основную инициализацию глобального логгера и набор глобальных функций логирования:

* `Setup()`
* `GetLogger()`
* `WithContext()`
* `Debug()` / `Debugf()`
* `Info()` / `Infof()`
* `Warn()` / `Warnf()`
* `Error()` / `Errorf()`
* `Fatal()` / `Fatalf()`
* `Panic()` / `Panicf()`

### `pkg/logger/config.go`

Содержит:

* структуру `Config`;
* `DefaultConfig()`;
* `LoadFromEnv()`.

### `pkg/logger/formatter.go`

Содержит кастомный `ColorFormatter` для цветного вывода в консоль с указанием:

* времени;
* уровня логирования;
* caller;
* текстового сообщения;
* структурированных полей.

### `pkg/logger/hooks.go`

Содержит `ErrorFileHook`, предназначенный для записи ошибок в отдельный файл.

### `pkg/logger/telegram.go`

Содержит `TelegramHook` и логику отправки критических сообщений в Telegram.

Текущая реализация использует отправку для событий уровней:

* `FATAL`
* `PANIC`

### `pkg/logger/utils.go`

Содержит вспомогательные функции:

* `LogTestStart()`
* `LogTestEnd()`
* `LogCommand()`
* `LogResponse()`
* `WithUserContext()`
* `WithRequestContext()`
* `WithVPNContext()`

### `pkg/logger/gorm.go`

Содержит адаптер `GormLogger` для интеграции с GORM и логирования SQL-запросов.

### `pkg/logger/gin.go`

Содержит middleware для Gin и функции получения логгера и `request_id` из контекста HTTP-запроса.

## Поддерживаемые сценарии

### Консольное логирование

Поддерживается вывод в консоль с цветовым форматированием уровней.

### Файловое логирование

Поддерживается запись в общий лог-файл и отдельный файл ошибок.

### Ротация файлов

Ротация реализована через `lumberjack`.

### Контекстные поля

Поддерживается формирование логов с полями контекста, включая:

* `module`
* `user_id`
* `request_id`
* HTTP и VPN-параметры

### Gin

Поддерживается middleware для логирования HTTP-запросов.

### GORM

Поддерживается логирование обычных, медленных и ошибочных SQL-запросов.

### Telegram

Поддерживается отправка критических событий в Telegram-чат или топик при наличии соответствующей конфигурации.

## Конфигурация

Пакет использует следующие настройки:

```env
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

## Файлы логов

При включенном файловом логировании используются:

```text
logs/
├── spirit_vpn.log
└── spirit_vpn_error.log
```

## Использование

### Базовая инициализация

```go
cfg := logger.DefaultConfig()
if err := logger.Setup(cfg); err != nil {
    log.Fatal(err)
}
```

### Загрузка из окружения

```go
cfg := logger.LoadFromEnv()
if err := logger.Setup(cfg); err != nil {
    log.Fatal(err)
}
```

### Получение логгера с модулем

```go
log := logger.GetLogger("api.server")
log.Info("server started")
```

### Контекст пользователя

```go
logger.WithUserContext(12345).Info("user action")
```

### Контекст VPN

```go
logger.WithVPNContext(12345, "user@example.com").Info("vpn connected")
```

## Интеграция в проект

При подключении логгера в основные сервисы рекомендуется:

1. вызывать `logger.Setup()` в `main()`;
2. использовать `GetLogger()` с именем модуля;
3. добавлять контекстные поля для пользовательских, HTTP и VPN-операций;
4. использовать `GinMiddleware()` в API-сервере;
5. использовать `NewGormLogger()` в слое базы данных.

## Тестирование

Для пакета предусмотрены unit-тесты:

* `pkg/logger/logger_test.go`
* `pkg/logger/telegram_test.go`

Запуск:

```bash
go test ./pkg/logger/...
```

## Связанные документы

* `pkg/logger/README.md`
* `docs/LOGGER_MIGRATION.md`
* `docs/LOGGER_ERRORS.md`
