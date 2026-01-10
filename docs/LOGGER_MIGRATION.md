# Миграция на новый логгер

Руководство по замене стандартного `log` на структурированный логгер `pkg/logger`.

## Быстрая миграция

### 1. Инициализация в main.go

**Было:**
```go
package main

import "log"

func main() {
    log.Println("Starting application...")
    // ...
}
```

**Стало:**
```go
package main

import (
    "github.com/RomanRyabinkin/SpiritVPN/pkg/config"
    "github.com/RomanRyabinkin/SpiritVPN/pkg/logger"
)

func main() {
    cfg, err := config.Load()
    if err != nil {
        panic(err)
    }

    loggerConfig := &logger.Config{
        Level:         cfg.Logger.Level,
        LogDir:        cfg.Logger.LogDir,
        ConsoleOutput: cfg.Logger.ConsoleOutput,
        FileOutput:    cfg.Logger.FileOutput,
        ColoredOutput: cfg.Logger.ColoredOutput,
        ErrorLogFile:  cfg.Logger.ErrorLogFile,
        Enabled:       cfg.Logger.Enabled,
        MaxFileSize:   cfg.Logger.MaxFileSize,
        MaxBackups:    cfg.Logger.MaxBackups,
        MaxAge:        cfg.Logger.MaxAge,
    }

    if err := logger.Setup(loggerConfig); err != nil {
        panic(err)
    }

    logger.Info("Starting application...")
    // ...
}
```

### 2. Замена импортов и вызовов

**Было:**
```go
import "log"

log.Printf("User %d connected", userID)
log.Println("Processing request")
log.Fatalf("Database error: %v", err)
```

**Стало:**
```go
import "github.com/RomanRyabinkin/SpiritVPN/pkg/logger"

logger.Infof("User %d connected", userID)
logger.Info("Processing request")
logger.Fatalf("Database error: %v", err)
```

## Примеры миграции по файлам

### internal/vpn/server.go

**Было:**
```go
package vpn

import (
    "log"
)

func (s *Server) Start() error {
    log.Printf("Starting VPN server integration on port %d", s.config.VPN.Port)

    // Connect to Xray
    if err := s.xray.Connect(); err != nil {
        log.Printf("Failed to connect to Xray API: %v", err)
        return err
    }

    log.Println("Connected to Xray API")
    return nil
}
```

**Стало:**
```go
package vpn

import (
    "github.com/RomanRyabinkin/SpiritVPN/pkg/logger"
    "github.com/sirupsen/logrus"
)

var log = logger.GetLogger("vpn.server")

func (s *Server) Start() error {
    log.Infof("Starting VPN server integration on port %d", s.config.VPN.Port)

    // Connect to Xray
    if err := s.xray.Connect(); err != nil {
        log.WithError(err).Error("Failed to connect to Xray API")
        return err
    }

    log.Info("Connected to Xray API")
    return nil
}

func (s *Server) AddUser(userID int, email string) error {
    userLog := log.WithFields(logrus.Fields{
        "user_id": userID,
        "email":   email,
    })

    userLog.Info("Adding new VPN user")

    if err := s.xray.AddUser(email, uuid); err != nil {
        userLog.WithError(err).Error("Failed to add user")
        return err
    }

    userLog.Info("User successfully added")
    return nil
}
```

### internal/database/database.go

**Было:**
```go
package database

import (
    "log"
    "gorm.io/gorm/logger"
)

func Connect(cfg *config.DatabaseConfig) (*gorm.DB, error) {
    dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s",
        cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Name)

    db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
        Logger: logger.Default.LogMode(logger.Info),
    })

    if err != nil {
        return nil, err
    }

    log.Println("Successfully connected to database via GORM")
    return db, nil
}
```

**Стало:**
```go
package database

import (
    "github.com/RomanRyabinkin/SpiritVPN/pkg/logger"
    "gorm.io/gorm/logger"
)

var log = logger.GetLogger("database")

func Connect(cfg *config.DatabaseConfig) (*gorm.DB, error) {
    log.WithFields(logrus.Fields{
        "host": cfg.Host,
        "port": cfg.Port,
        "user": cfg.User,
        "name": cfg.Name,
    }).Info("Connecting to PostgreSQL database")

    dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s",
        cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Name)

    // Интеграция с GORM logger
    gormLogger := NewGormLogger()

    db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
        Logger: gormLogger,
    })

    if err != nil {
        log.WithError(err).Error("Failed to connect to database")
        return nil, err
    }

    log.Info("Successfully connected to database via GORM")
    return db, nil
}

// NewGormLogger создает адаптер для интеграции с GORM
func NewGormLogger() logger.Interface {
    return &GormLogger{
        log: logger.GetLogger("gorm"),
    }
}

type GormLogger struct {
    log *logrus.Entry
}

func (l *GormLogger) LogMode(level logger.LogLevel) logger.Interface {
    return l
}

func (l *GormLogger) Info(ctx context.Context, msg string, data ...interface{}) {
    l.log.Infof(msg, data...)
}

func (l *GormLogger) Warn(ctx context.Context, msg string, data ...interface{}) {
    l.log.Warnf(msg, data...)
}

func (l *GormLogger) Error(ctx context.Context, msg string, data ...interface{}) {
    l.log.Errorf(msg, data...)
}

func (l *GormLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
    elapsed := time.Since(begin)
    sql, rows := fc()

    fields := logrus.Fields{
        "elapsed": elapsed,
        "rows":    rows,
        "sql":     sql,
    }

    if err != nil {
        l.log.WithFields(fields).WithError(err).Error("SQL query failed")
    } else if elapsed > 200*time.Millisecond {
        l.log.WithFields(fields).Warn("Slow SQL query")
    } else {
        l.log.WithFields(fields).Debug("SQL query executed")
    }
}
```

### test/smoke/xray_test.go

**Было:**
```go
package smoke

import (
    "log"
    "testing"
)

func TestXrayIntegration(t *testing.T) {
    log.Println("[INFO] Starting Xray integration test")

    client, err := xray.Connect(...)
    if err != nil {
        log.Fatalf("[ERROR] Failed to connect to Xray API: %v", err)
    }

    log.Printf("[INFO] Adding test user: %s", testEmail)
    // ...
}
```

**Стало:**
```go
package smoke

import (
    "testing"
    "time"

    "github.com/RomanRyabinkin/SpiritVPN/pkg/logger"
    "github.com/sirupsen/logrus"
)

func TestXrayIntegration(t *testing.T) {
    logger.Setup(&logger.Config{
        Level:         "debug",
        ConsoleOutput: true,
        FileOutput:    false,
        ColoredOutput: true,
        Enabled:       true,
    })

    log := logger.GetLogger("test.xray")

    start := time.Now()
    logger.LogTestStart(log, "XrayIntegration", map[string]interface{}{
        "api_address": testAPIAddress,
        "test_email":  testEmail,
    })

    client, err := xray.Connect(...)
    if err != nil {
        log.WithError(err).Fatal("Failed to connect to Xray API")
    }

    log.WithField("email", testEmail).Info("Adding test user")

    // ... выполнение теста ...

    status := "PASS"
    if t.Failed() {
        status = "FAIL"
    }
    logger.LogTestEnd(log, "XrayIntegration", status, time.Since(start))
}
```

## Паттерны использования

### 1. Модульные логгеры

Создавайте отдельный логгер для каждого модуля:

```go
// В начале файла
var log = logger.GetLogger("module.name")

// В функциях
func DoSomething() {
    log.Info("Doing something")
}
```

### 2. Контекстные поля

Добавляйте контекст к логам:

```go
userLog := log.WithFields(logrus.Fields{
    "user_id": userID,
    "email":   email,
})

userLog.Info("User logged in")
userLog.WithField("ip", ipAddress).Info("Connection from IP")
```

### 3. Обработка ошибок

```go
if err != nil {
    log.WithError(err).Error("Operation failed")
    return err
}
```

### 4. HTTP middleware

```go
func LoggingMiddleware() gin.HandlerFunc {
    return func(c *gin.Context) {
        start := time.Now()
        requestID := uuid.New().String()

        // Создаение логгера для запроса
        reqLog := logger.WithRequestContext(
            c.Request.Method,
            c.Request.URL.Path,
            requestID,
        )

        c.Set("logger", reqLog)
        c.Set("request_id", requestID)

        reqLog.Info("Request started")

        c.Next()

        latency := time.Since(start)
        statusCode := c.Writer.Status()

        logEntry := reqLog.WithFields(logrus.Fields{
            "status":  statusCode,
            "latency": latency,
            "ip":      c.ClientIP(),
        })

        if statusCode >= 500 {
            logEntry.Error("Request failed")
        } else if statusCode >= 400 {
            logEntry.Warn("Request completed with client error")
        } else {
            logEntry.Info("Request completed successfully")
        }
    }
}
```

## Чек-лист миграции

- [ ] Добавить `logger.Setup()` в `main.go`
- [ ] Заменить `import "log"` на `import "github.com/RomanRyabinkin/SpiritVPN/pkg/logger"`
- [ ] Заменить `log.Printf` на `logger.Infof`
- [ ] Заменить `log.Println` на `logger.Info`
- [ ] Заменить `log.Fatalf` на `logger.Fatalf`
- [ ] Добавить контекстные поля где это уместно
- [ ] Создать модульные логгеры (`var log = logger.GetLogger("module.name")`)
- [ ] Обновить тесты для использования нового логгера
- [ ] Настроить `.env` файл с конфигурацией логирования
- [ ] Проверить ротацию файлов (должна создаться директория `./logs`)

## Дополнительные возможности

### Performance profiling

```go
func SlowOperation() {
    start := time.Now()
    defer func() {
        logger.WithField("duration", time.Since(start)).Debug("SlowOperation completed")
    }()

    // ... операция ...
}
```

### Conditional logging

```go
if logger.Log.IsLevelEnabled(logrus.DebugLevel) {
    expensiveDebugInfo := calculateDebugInfo()
    logger.Debugf("Debug info: %v", expensiveDebugInfo)
}
```

## Troubleshooting

### Логи не появляются в файлах

Проверьте:
1. `FileOutput: true` в конфигурации
2. Директория `LogDir` существует и доступна для записи
3. `Enabled: true` в конфигурации

### Нет цветного вывода

Проверьте:
1. `ColoredOutput: true` в конфигурации
2. Терминал поддерживает ANSI цвета
3. `ConsoleOutput: true` в конфигурации

### Telegram уведомления не работают

Проверьте:
1. Правильность `TelegramBotToken` и `TelegramChatID`
2. Уровень логирования (отправляются только Fatal и Panic)
3. Бот добавлен в чат и имеет права на отправку сообщений
