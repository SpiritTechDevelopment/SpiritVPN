# Logger Package

Структурированная система логирования для SpiritVPN с использованием `logrus` и `lumberjack`.

## Возможности

- **Цветной вывод** в консоль с ANSI цветами
- **Ротация файлов** с автоматическим сжатием старых логов
- **Уровни логирования**: Debug, Info, Warning, Error, Fatal, Panic
- **Структурированное логирование** с контекстными полями
- **Отдельный файл для ошибок** (error.log)
- **Telegram уведомления** для критических ошибок
- **Информация о вызывающем коде** (файл:строка:функция)

## Быстрый старт

### 1. Инициализация логгера

```go
package main

import (
    "github.com/RomanRyabinkin/SpiritVPN/pkg/logger"
)

func main() {
    // Использование конфигурации по умолчанию
    logger.Setup(logger.DefaultConfig())

    // Или кастомная конфигурация
    config := &logger.Config{
        Level:         "info",
        LogDir:        "./logs",
        ConsoleOutput: true,
        FileOutput:    true,
        ColoredOutput: true,
    }
    logger.Setup(config)
}
```

### 2. Базовое использование

```go
// Простое логирование
logger.Info("Application started")
logger.Debug("Debug information")
logger.Warn("Warning message")
logger.Error("Error occurred")

// Форматированное логирование
logger.Infof("User %s connected from %s", username, ip)
logger.Errorf("Failed to connect to database: %v", err)
```

### 3. Логирование с контекстом

```go
import "github.com/sirupsen/logrus"

// Получение логгера с контекстными полями
log := logger.GetLogger("vpn.server", logrus.Fields{
    "user_id": 123,
    "email":   "user@example.com",
})

log.Info("User connected to VPN")
log.WithField("bandwidth", "100MB/s").Info("Traffic info")
```

### 4. Специализированные контексты

```go
// Контекст пользователя
userLog := logger.WithUserContext(12345)
userLog.Info("User logged in")

// Контекст VPN
vpnLog := logger.WithVPNContext(12345, "user@example.com")
vpnLog.Info("VPN connection established")

// Контекст HTTP запроса
reqLog := logger.WithRequestContext("GET", "/api/users", "req-123")
reqLog.Info("Handling request")
```

### 5. Утилитные функции для тестов

```go
import "time"

log := logger.GetLogger("test.smoke")

// Логирование начала теста
logger.LogTestStart(log, "TestVPNConnection", map[string]interface{}{
    "server": "vpn.example.com",
    "port":   443,
})

// ... выполнение теста ...

// Логирование окончания теста
logger.LogTestEnd(log, "TestVPNConnection", "PASS", 2*time.Second)

// Логирование команд
logger.LogCommand(log, "xray -test -config config.json")

// Логирование HTTP ответов
logger.LogResponse(log, 200, responseBody, 500)
```

## Конфигурация

### Из кода

```go
config := &logger.Config{
    Level:            "info",        // debug, info, warning, error, fatal, panic
    LogDir:           "./logs",       // Директория для логов
    ConsoleOutput:    true,           // Вывод в консоль
    FileOutput:       true,           // Запись в файл
    ColoredOutput:    true,           // Цветной вывод
    ErrorLogFile:     true,           // Отдельный файл для ошибок
    Enabled:          true,           // Включить логирование
    TimestampFormat:  time.RFC3339,   // Формат времени
    MaxFileSize:      10,             // Макс. размер файла (МБ)
    MaxBackups:       5,              // Кол-во бэкапов
    MaxAge:           30,             // Дни хранения
    TelegramBotToken: "bot_token",    // Токен Telegram бота
    TelegramChatID:   "chat_id",      // ID чата
}

logger.Setup(config)
```

### Из переменных окружения

Добавьте в `.env` файл:

```env
# Настройки логирования
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

# Telegram уведомления (опционально)
LOG_TELEGRAM_BOT_TOKEN=your_bot_token
LOG_TELEGRAM_CHAT_ID=your_chat_id
```

Затем в коде:

```go
import "github.com/RomanRyabinkin/SpiritVPN/pkg/config"

cfg, _ := config.Load()

loggerConfig := &logger.Config{
    Level:            cfg.Logger.Level,
    LogDir:           cfg.Logger.LogDir,
    ConsoleOutput:    cfg.Logger.ConsoleOutput,
    FileOutput:       cfg.Logger.FileOutput,
    ColoredOutput:    cfg.Logger.ColoredOutput,
    ErrorLogFile:     cfg.Logger.ErrorLogFile,
    Enabled:          cfg.Logger.Enabled,
    MaxFileSize:      cfg.Logger.MaxFileSize,
    MaxBackups:       cfg.Logger.MaxBackups,
    MaxAge:           cfg.Logger.MaxAge,
    TelegramBotToken: cfg.Logger.TelegramBotToken,
    TelegramChatID:   cfg.Logger.TelegramChatID,
}

logger.Setup(loggerConfig)
```

## Структура файлов

```
logs/
├── spirit_vpn.log          # Основной лог-файл
├── spirit_vpn.log.1.gz     # Ротированные архивы
├── spirit_vpn.log.2.gz
├── spirit_vpn_error.log    # Только ошибки (ERROR, FATAL, PANIC)
└── spirit_vpn_error.log.1.gz
```

## Цветной вывод

Логи в консоли отображаются с цветами:

- **DEBUG** - Cyan (голубой)
- **INFO** - Green (зеленый)
- **WARNING** - Yellow (желтый)
- **ERROR** - Red (красный)
- **FATAL/PANIC** - Magenta (пурпурный)

Пример вывода:
```
[2024-01-10T15:04:05Z] [INFO   ] [server.go:42:Start] Application started {module=api.server, version=1.0.0}
[2024-01-10T15:04:06Z] [WARNING] [handler.go:123:Handle] Rate limit exceeded {user_id=123, ip=192.168.1.1}
[2024-01-10T15:04:07Z] [ERROR  ] [db.go:56:Connect] Failed to connect to database {error=connection timeout}
```

## Telegram уведомления

Критические ошибки (FATAL, PANIC) автоматически отправляются в Telegram:

```go
config := &logger.Config{
    TelegramBotToken: "123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11",
    TelegramChatID:   "-1001234567890",
    // ... другие настройки
}

logger.Setup(config)

// Это отправит уведомление в Telegram
logger.Fatal("Critical database failure!")
```

## Примеры для разных компонентов

### API Server

```go
log := logger.GetLogger("api.server")

// Старт сервера
log.Info("Starting API server on :8080")

// HTTP запросы
reqLog := logger.WithRequestContext(
    r.Method,
    r.URL.Path,
    requestID,
)
reqLog.Info("Handling request")
```

### VPN Server

```go
log := logger.GetLogger("vpn.server")

// Подключение пользователя
vpnLog := logger.WithVPNContext(userID, email)
vpnLog.Info("User connecting to VPN")

// Статистика трафика
vpnLog.WithFields(logrus.Fields{
    "received": receivedBytes,
    "sent":     sentBytes,
}).Info("Traffic stats")
```

### Telegram Bot

```go
log := logger.GetLogger("bot.handler")

// Обработка команды
log.WithFields(logrus.Fields{
    "user_id":  update.Message.From.ID,
    "username": update.Message.From.UserName,
    "command":  update.Message.Text,
}).Info("Processing command")
```

### Database

```go
log := logger.GetLogger("database")

// Подключение
log.Info("Connecting to PostgreSQL")

// Ошибка соединения
log.WithFields(logrus.Fields{
    "host": cfg.Database.Host,
    "port": cfg.Database.Port,
}).Error("Failed to connect to database")
```

## Тестирование

```bash
# Запуск тестов
go test ./pkg/logger/...

# С выводом логов
go test -v ./pkg/logger/...

# С покрытием
go test -cover ./pkg/logger/...
```


## Best Practices

1. **Инициализируйте один раз** - вызывайте `logger.Setup()` в `main()` функции
2. **Используйте контекстные поля** - добавляйте `user_id`, `request_id` и другие идентификаторы
3. **Правильные уровни**:
   - `Debug` - отладочная информация для разработки
   - `Info` - нормальная работа приложения
   - `Warn` - предупреждения, не критичные проблемы
   - `Error` - ошибки, требующие внимания
   - `Fatal` - критические ошибки, приложение не может продолжить работу
4. **Структурированные данные** - используйте поля вместо форматированных строк
5. **Не логируйте чувствительные данные** - пароли, токены, ключи API

## License

MIT
