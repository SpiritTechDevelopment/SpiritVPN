# Logger Package

## Назначение

Пакет `pkg/logger` предоставляет структурированную систему логирования для компонентов SpiritVPN на базе `logrus` и `lumberjack`.

Пакет предназначен для:

* консольного и файлового логирования;
* разделения общих логов и логов ошибок;
* использования контекстных полей;
* интеграции с Gin;
* интеграции с GORM;
* отправки критических уведомлений в Telegram.

## Возможности

* настройка уровня логирования;
* цветной вывод в консоль;
* запись логов в файл с ротацией;
* отдельный файл для ошибок;
* форматирование логов с указанием caller;
* контекстные логгеры для пользователя, VPN и HTTP-запросов;
* middleware для Gin;
* адаптер логирования для GORM;
* Telegram hook для критических событий.

## Структура пакета

```text
pkg/logger/
├── README.md
├── config.go
├── doc.go
├── formatter.go
├── gin.go
├── gorm.go
├── hooks.go
├── logger.go
├── logger_test.go
├── telegram.go
├── telegram_test.go
└── utils.go
```

## Быстрый старт

### Инициализация

```go
package main

import (
    "log"

    "github.com/RomanRyabinkin/SpiritVPN/pkg/logger"
)

func main() {
    if err := logger.Setup(logger.DefaultConfig()); err != nil {
        log.Fatal(err)
    }

    logger.Info("application started")
}
```

### Кастомная конфигурация

```go
cfg := &logger.Config{
    Level:           "info",
    LogDir:          "./logs",
    ConsoleOutput:   true,
    FileOutput:      true,
    ColoredOutput:   true,
    ErrorLogFile:    true,
    Enabled:         true,
    TimestampFormat: time.RFC3339,
    MaxFileSize:     10,
    MaxBackups:      5,
    MaxAge:          30,
}

if err := logger.Setup(cfg); err != nil {
    log.Fatal(err)
}
```

## Основные функции

### Глобальные функции

Пакет предоставляет глобальные функции логирования:

* `Debug()` / `Debugf()`
* `Info()` / `Infof()`
* `Warn()` / `Warnf()`
* `Error()` / `Errorf()`
* `Fatal()` / `Fatalf()`
* `Panic()` / `Panicf()`

### Получение логгера с контекстом

```go
log := logger.GetLogger("api.server")
log.Info("server started")
```

С дополнительными полями:

```go
log := logger.GetLogger("vpn.server", logrus.Fields{
    "user_id": 123,
    "email":   "user@example.com",
})

log.Info("user connected")
```

### Универсальный контекст

```go
entry := logger.WithContext(logrus.Fields{
    "request_id": "req-123",
    "component":  "api",
})

entry.Info("request received")
```

## Специализированные контекстные функции

### Пользовательский контекст

```go
userLog := logger.WithUserContext(12345)
userLog.Info("user logged in")
```

### Контекст HTTP-запроса

```go
reqLog := logger.WithRequestContext("GET", "/health", "req-123")
reqLog.Info("request started")
```

### Контекст VPN

```go
vpnLog := logger.WithVPNContext(12345, "user@example.com")
vpnLog.Info("vpn session started")
```

## Конфигурация

### Структура Config

```go
type Config struct {
    Level            string
    LogDir           string
    ConsoleOutput    bool
    FileOutput       bool
    ColoredOutput    bool
    ErrorLogFile     bool
    Enabled          bool
    TimestampFormat  string
    MaxFileSize      int
    MaxBackups       int
    MaxAge           int
    TelegramBotToken string
    TelegramChatID   string
    TelegramThreadID string
}
```

### Конфигурация по умолчанию

`logger.DefaultConfig()` возвращает конфигурацию со следующими параметрами:

* уровень `info`;
* директория `./logs`;
* вывод в консоль включен;
* запись в файл включена;
* цветной вывод включен;
* отдельный файл ошибок включен.

### Загрузка из переменных окружения

Для загрузки конфигурации логгера можно использовать `logger.LoadFromEnv()`.

Поддерживаются переменные:

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

Пример:

```go
cfg := logger.LoadFromEnv()
if err := logger.Setup(cfg); err != nil {
    log.Fatal(err)
}
```

## Формат вывода

При включенном цветном формате в консоли используются следующие цвета уровней:

* `DEBUG` — cyan
* `INFO` — green
* `WARN` — yellow
* `ERROR` — red
* `FATAL` / `PANIC` — magenta

Формат записи включает:

* временную метку;
* уровень логирования;
* caller (`file:line:function`);
* сообщение;
* структурированные поля.

Пример:

```text
[2026-01-10T15:04:05Z] [INFO   ] [server.go:42:Start] Application started {module=api.server}
```

## Файлы логов

При включенном файловом логировании создаются:

```text
logs/
├── spirit_vpn.log
└── spirit_vpn_error.log
```

### Назначение файлов

* `spirit_vpn.log` — общий лог;
* `spirit_vpn_error.log` — записи уровней `ERROR`, `FATAL`, `PANIC`.

Ротация логов выполняется через `lumberjack`.

## Telegram-уведомления

Пакет поддерживает `TelegramHook`.

### Назначение

Telegram hook предназначен для отправки критических событий в Telegram-чат или топик.

### Поддерживаемые параметры

* `TelegramBotToken`
* `TelegramChatID`
* `TelegramThreadID`

### Поведение текущей реализации

В текущей реализации отправка сообщений в Telegram выполняется для событий уровней:

* `FATAL`
* `PANIC`

### Пример настройки

```go
cfg := &logger.Config{
    Level:            "info",
    LogDir:           "./logs",
    ConsoleOutput:    true,
    FileOutput:       true,
    ColoredOutput:    true,
    ErrorLogFile:     true,
    Enabled:          true,
    TimestampFormat:  time.RFC3339,
    TelegramBotToken: os.Getenv("LOG_TELEGRAM_BOT_TOKEN"),
    TelegramChatID:   os.Getenv("LOG_TELEGRAM_CHAT_ID"),
    TelegramThreadID: os.Getenv("LOG_TELEGRAM_THREAD_ID"),
}

if err := logger.Setup(cfg); err != nil {
    log.Fatal(err)
}
```

### Ручное добавление hook

```go
hook := logger.NewTelegramHook(botToken, chatID, threadID, "api-server")
logger.Log.AddHook(hook)
```

## Интеграция с GORM

Для интеграции с GORM используется адаптер `NewGormLogger()`.

```go
gormLogger := logger.NewGormLogger("database", 200*time.Millisecond)

db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
    Logger: gormLogger,
})
```

В адаптере поддерживаются:

* информационные сообщения;
* предупреждения;
* ошибки;
* логирование медленных SQL-запросов.

## Интеграция с Gin

Для HTTP-логирования используется middleware `GinMiddleware()`.

```go
router := gin.New()
router.Use(logger.GinMiddleware())
```

Middleware автоматически добавляет:

* `request_id`;
* HTTP method;
* path;
* IP клиента;
* user agent;
* статус ответа;
* latency;
* размер ответа.

Извлечение логгера из контекста:

```go
func handler(c *gin.Context) {
    log := logger.GetLoggerFromGinContext(c)
    log.Info("processing request")
}
```

## Вспомогательные функции

Пакет содержит дополнительные функции:

* `LogTestStart()`
* `LogTestEnd()`
* `LogCommand()`
* `LogResponse()`
* `GetRequestID()`

## Отключение логирования

Если `Enabled=false`, пакет переводит вывод логов в `io.Discard`.

## Тестирование

Запуск тестов пакета:

```bash
go test ./pkg/logger/...
```

С подробным выводом:

```bash
go test -v ./pkg/logger/...
```

С покрытием:

```bash
go test -cover ./pkg/logger/...
```

## Практические рекомендации

1. Инициализировать логгер один раз при старте приложения
2. Использовать контекстные поля вместо длинных форматированных строк
3. Не логировать секреты, токены и приватные ключи
4. Для production включать файловое логирование и ротацию
5. Для HTTP и SQL использовать готовые интеграции пакета
