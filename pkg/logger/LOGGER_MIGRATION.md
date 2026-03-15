# Logger Migration Guide

## Назначение

Документ описывает порядок перехода компонентов SpiritVPN на использование пакета `pkg/logger`.

## Цель миграции

Переход на единый пакет логирования позволяет:

* использовать общий формат логов;
* унифицировать уровни логирования;
* внедрить файловое логирование и ротацию;
* использовать структурированные поля;
* интегрировать логирование с Gin и GORM;
* при необходимости отправлять критические события в Telegram.

## Базовый подход

Миграция выполняется поэтапно:

1. инициализация `pkg/logger` в точках входа приложений;
2. замена стандартного `log` на `pkg/logger` в коде компонентов;
3. перевод строковых логов на структурированные записи;
4. подключение специализированных интеграций для Gin и GORM.

## Шаг 1. Инициализация логгера в `main()`

В каждой точке входа необходимо вызывать `logger.Setup()`.

Пример:

```go
cfg := logger.LoadFromEnv()
if err := logger.Setup(cfg); err != nil {
    log.Fatal(err)
}
```

Если используется конфигурация из `pkg/config`, можно собрать `logger.Config` из `cfg.Logger`.

## Шаг 2. Замена стандартного логирования

### До миграции

```go
import "log"

log.Println("server started")
log.Printf("user %d connected", userID)
```

### После миграции

```go
import "github.com/RomanRyabinkin/SpiritVPN/pkg/logger"

logger.Info("server started")
logger.Infof("user %d connected", userID)
```

## Шаг 3. Использование модульных логгеров

Для компонентов рекомендуется использовать `GetLogger()`.

```go
log := logger.GetLogger("api.server")
log.Info("server started")
```

## Шаг 4. Переход на структурированные поля

### До миграции

```go
logger.Infof("user %d connected from %s", userID, ip)
```

### После миграции

```go
log := logger.GetLogger("vpn.server")
log.WithFields(logrus.Fields{
    "user_id": userID,
    "ip":      ip,
}).Info("user connected")
```

## Шаг 5. Использование готовых контекстов

Для типовых сценариев доступны:

* `WithUserContext()`
* `WithRequestContext()`
* `WithVPNContext()`

Примеры:

```go
logger.WithUserContext(userID).Info("user action")
logger.WithRequestContext("GET", "/health", requestID).Info("request started")
logger.WithVPNContext(userID, email).Info("vpn connected")
```

## Шаг 6. Подключение к Gin

В API-сервере рекомендуется использовать `GinMiddleware()`.

```go
router := gin.New()
router.Use(logger.GinMiddleware())
```

В обработчиках можно извлекать логгер из контекста:

```go
reqLog := logger.GetLoggerFromGinContext(c)
reqLog.Info("processing request")
```

## Шаг 7. Подключение к GORM

Для слоя данных следует использовать `NewGormLogger()`.

```go
gormLogger := logger.NewGormLogger("database", 200*time.Millisecond)

db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
    Logger: gormLogger,
})
```

## Шаг 8. Включение файлового логирования

Для production-режима рекомендуется включать:

* `FileOutput=true`
* `ErrorLogFile=true`
* корректные значения `MaxFileSize`, `MaxBackups`, `MaxAge`

## Шаг 9. Подключение Telegram hook

При необходимости можно включить Telegram-уведомления через:

* `LOG_TELEGRAM_BOT_TOKEN`
* `LOG_TELEGRAM_CHAT_ID`
* `LOG_TELEGRAM_THREAD_ID`

## Контрольный список миграции

Перед завершением миграции рекомендуется проверить:

* логгер инициализируется в `main()`;
* стандартный `log` заменен в основных компонентах;
* модульные логгеры используются в API, DB, VPN и bot-слоях;
* HTTP-слой использует `GinMiddleware()`;
* DB-слой использует `NewGormLogger()`;
* файловое логирование включается конфигурацией;
* критические события не содержат секретов и приватных данных.

## Рекомендации

1. Выполнять миграцию по компонентам, а не одновременно по всему проекту
2. Переводить длинные строковые логи в структурированные поля
3. Исключать из логов токены, ключи и чувствительные данные
4. Сохранять единый набор имен модулей и полей во всех сервисах
