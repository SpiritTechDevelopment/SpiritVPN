# История изменений

Все важные изменения в этом проекте будут документироваться в этом файле.

Формат основан на [Keep a Changelog](https://keepachangelog.com/ru/1.0.0/),
и этот проект следует [Semantic Versioning](https://semver.org/lang/ru/).

## [Unreleased]

### Добавлено
- Пакет структурированного логирования с интеграцией logrus и lumberjack
- Цветной вывод в консоль с ANSI кодами
- Автоматическая ротация лог-файлов (10MB, 5 бэкапов, 30 дней)
- Отдельный файл для ошибок (error.log)
- Telegram уведомления для критических ошибок с категоризацией по компонентам
- Поддержка топиков/тредов Telegram (message_thread_id)
- GORM адаптер для логирования SQL запросов
- Gin middleware для логирования HTTP запросов
- Загрузка конфигурации из переменных окружения
- CI/CD workflow для отчетов о покрытии в топик CI (Thread ID: 18)
- Workflow уведомлений о PR в топик Review (Thread ID: 20)
- Workflow ежедневной сводки по PR
- Категоризация ошибок по компонентам (API, BOT, VPN, INFRA, DB)

### Изменено
- Обновлен логгер для использования специфичных топиков Telegram

### Исправлено
- Исправлены ошибки errcheck линтера в пакете logger
- Исправлены конфликты функции main в файлах примеров
- Исправлены import paths в примерах
- Добавлены permissions для workflow changelog (2eaa2b2)

### Документация

## [0.1.0] - 2026-01-10

### Добавлено
- Начальная структура проекта
- Базовая реализация VPN сервера
- Интеграция Telegram бота
- Настройка API сервера
- Интеграция PostgreSQL базы данных
- Интеграция Xray VPN ядра

[Unreleased]: https://github.com/RomanRyabinkin/SpiritVPN/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/RomanRyabinkin/SpiritVPN/releases/tag/v0.1.0
