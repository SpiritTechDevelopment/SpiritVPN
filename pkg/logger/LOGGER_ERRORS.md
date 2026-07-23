# Logger Errors Guide

## Назначение

Документ описывает использование Telegram-уведомлений и обработку критических событий в пакете `pkg/logger`.

## Общая схема

Пакет `pkg/logger` поддерживает Telegram hook для отправки критических событий в Telegram-чат или отдельный топик супергруппы.

Текущая реализация основана на типе:

* `TelegramHook`

## Поддерживаемые параметры конфигурации

Для включения Telegram-уведомлений используются переменные:

```env
LOG_TELEGRAM_BOT_TOKEN=
LOG_TELEGRAM_CHAT_ID=
LOG_TELEGRAM_THREAD_ID=
```

### Описание параметров

* `LOG_TELEGRAM_BOT_TOKEN` — токен Telegram-бота для отправки сообщений;
* `LOG_TELEGRAM_CHAT_ID` — ID чата или супергруппы;
* `LOG_TELEGRAM_THREAD_ID` — ID топика в супергруппе, если требуется отправка в конкретный thread.

## Поведение текущей реализации

В текущем коде hook регистрируется при наличии `TelegramBotToken` и `TelegramChatID`.

Сообщения отправляются для событий уровней:

* `FATAL`
* `PANIC`

Критические события уровня `ERROR` проходят через уровни hook, но фактическая отправка в текущей реализации ограничена уровнями `FATAL` и `PANIC`.

## Формат сообщения

Telegram-сообщение включает:

* уровень события;
* время события;
* модуль, если поле `module` присутствует;
* `user_id`, если поле присутствует;
* текст сообщения;
* дополнительные поля контекста.

## Компонентные префиксы

Hook поддерживает указание имени компонента через параметр `component`.

Поддерживаемые префиксы:

* `api-server` → `API`
* `vpn-server` → `VPN`
* `infrastructure` → `INFRA`
* `database` → `DB`

## Пример настройки через `logger.Config`

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

## Пример ручного подключения hook

```go
hook := logger.NewTelegramHook(
    os.Getenv("LOG_TELEGRAM_BOT_TOKEN"),
    os.Getenv("LOG_TELEGRAM_CHAT_ID"),
    os.Getenv("LOG_TELEGRAM_THREAD_ID"),
    "api-server",
)

logger.Log.AddHook(hook)
```

## Пример критического события

```go
logger.Fatal("database connection failed")
```

При корректной настройке hook событие будет отправлено в Telegram.

## Ограничения текущей реализации

Следует учитывать, что:

* отправка выполняется синхронно через HTTP-клиент;
* таймаут клиента составляет 5 секунд;
* длина сообщения ограничивается 4000 символами;
* при статусе ответа Telegram API, отличном от `200 OK`, возвращается ошибка.

## Рекомендации по использованию

1. Использовать Telegram hook для действительно критических событий
2. Не включать в сообщения токены, ключи, пароли и персональные данные
3. Перед использованием в production проверить корректность `chat_id` и `thread_id`
4. Поддерживать единые имена компонентов во всех сервисах
5. Разделять общие ошибки и фатальные аварийные события по уровню логирования

## Диагностика проблем с отправкой

Если уведомления не отправляются, рекомендуется проверить:

* корректность `LOG_TELEGRAM_BOT_TOKEN`;
* корректность `LOG_TELEGRAM_CHAT_ID`;
* корректность `LOG_TELEGRAM_THREAD_ID`, если используется топик;
* сетевую доступность Telegram API;
* наличие событий уровня `FATAL` или `PANIC`.

## Связанные документы

* `pkg/logger/README.md`
* `docs/LOGGER_MIGRATION.md`
* `LOGGER_IMPLEMENTATION.md`
