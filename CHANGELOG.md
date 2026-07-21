# История изменений

Все значимые изменения проекта SpiritVPN фиксируются в этом файле.

Формат файла основан на **Keep a Changelog**, версия проекта ведется по принципам **Semantic Versioning**.

## [Unreleased]

### Изменено
- Feature/billing developing ([#21](https://github.com/SpiritTechDevelopment/SpiritVPN/pull/21)) by @RomanRyabinkin

### Документация
- feat: [DOCS-01] updated docs ([#20](https://github.com/SpiritTechDevelopment/SpiritVPN/pull/20)) by @RomanRyabinkin

### Добавлено

* Добавлен API health check эндпоинт (`/health`, `/health/advanced`) (#19) by @RomanRyabinkin
* Добавлен worker сбора статистики трафика (#18) by @RomanRyabinkin
* Добавлен пакет структурированного логирования на базе `logrus` и `lumberjack`
* Добавлен цветной консольный formatter для логов
* Добавлена ротация лог-файлов
* Добавлен отдельный файл логов ошибок
* Добавлена поддержка Telegram hook для критических событий
* Добавлена поддержка Telegram topics через `message_thread_id`
* Добавлен адаптер логирования для GORM
* Добавлено middleware логирования HTTP-запросов для Gin
* Добавлена загрузка конфигурации логгера из переменных окружения
* Добавлены workflow-уведомления для CI и review-процессов
* Добавлена категоризация критических событий по компонентам (`API`, `BOT`, `VPN`, `INFRA`, `DB`)

### Изменено
- Feature/billing developing ([#21](https://github.com/SpiritTechDevelopment/SpiritVPN/pull/21)) by @RomanRyabinkin

* Обновлена конфигурация Telegram hook для использования отдельных топиков
* Обновлена проектная документация и описание логирования

### Исправлено

* Исправлены ошибки `errcheck` в пакете `logger`
* Исправлены конфликты `main` в файлах примеров
* Исправлены import paths в примерах
* Добавлены permissions для workflow changelog (`2eaa2b2`)

## [0.1.0] - 2026-01-10

### Добавлено

* Добавлена начальная структура проекта
* Добавлена базовая реализация VPN-сервера
* Добавлена интеграция Telegram-бота
* Добавлена настройка API-сервера
* Добавлена интеграция PostgreSQL
* Добавлена интеграция Xray VPN-ядра

[Unreleased]: https://github.com/RomanRyabinkin/SpiritVPN/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/RomanRyabinkin/SpiritVPN/releases/tag/v0.1.0
