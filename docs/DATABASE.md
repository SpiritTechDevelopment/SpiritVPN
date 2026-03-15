# Database Architecture

## Общая информация

SpiritVPN использует PostgreSQL в качестве основной базы данных. Работа с данными реализована через GORM и внутренний слой `internal/database`.

В каталоге `internal/database` находятся:

* `database.go` — подключение и базовые операции инициализации;
* `models.go` — модели данных;
* `repository.go` — слой репозиториев;
* `repository_test.go` — unit-тесты репозиториев.

## Основные модели

В проекте определены следующие основные сущности:

1. **User** — пользователь системы
2. **Subscription** — подписка пользователя
3. **VPNConfig** — VPN-конфигурация пользователя
4. **VPNServer** — сервер VPN
5. **Payment** — запись о платеже
6. **TrafficStat** — статистика трафика
7. **SubscriptionPlan** — тарифный план

## Описание моделей

### User

Хранит идентификационные данные пользователя.

Ключевые поля:

* `ID`
* `TelegramID`
* `Username`
* `Email`
* `CreatedAt`
* `UpdatedAt`

Связи:

* `Subscriptions`
* `VPNConfigs`
* `Payments`
* `TrafficStats`

### Subscription

Описывает срок действия и параметры подписки.

Ключевые поля:

* `ID`
* `UserID`
* `PlanType`
* `StartDate`
* `EndDate`
* `IsActive`
* `AutoRenew`
* `CreatedAt`

Вспомогательные методы:

* `IsExpired()`
* `DaysLeft()`

### VPNConfig

Содержит параметры VPN-подключения, связанные с пользователем, подпиской и конкретным сервером.

Ключевые поля:

* `ID`
* `UserID`
* `SubscriptionID`
* `ServerID`
* `UUID`
* `Flow`
* `CreatedAt`
* `UpdatedAt`

### VPNServer

Описывает сервер VPN в инфраструктуре.

Ключевые поля:

* `ID`
* `Name`
* `Host`
* `Port`
* `PublicKey`
* `Location`
* `CountryCode`
* `IsActive`
* `MaxUsers`
* `CurrentUsers`
* `LoadPercent`
* `CreatedAt`
* `UpdatedAt`

Вспомогательные методы:

* `HasCapacity()`
* `UpdateLoad()`

### Payment

Хранит платежные записи и статус обработки оплаты.

Ключевые поля:

* `ID`
* `UserID`
* `SubscriptionID`
* `Amount`
* `Currency`
* `Status`
* `PaymentMethod`
* `TransactionID`
* `Metadata`
* `CreatedAt`
* `UpdatedAt`

Вспомогательные методы:

* `IsSuccessful()`
* `IsPending()`

### TrafficStat

Используется для хранения статистики трафика по пользователю и VPN-конфигурации.

Ключевые поля:

* `ID`
* `UserID`
* `ConfigID`
* `BytesSent`
* `BytesReceived`
* `Date`
* `CreatedAt`

Вспомогательные методы:

* `TotalBytes()`
* `TotalGB()`

### SubscriptionPlan

Описывает доступные тарифные планы.

Ключевые поля:

* `ID`
* `Name`
* `Code`
* `DurationDays`
* `Price`
* `Currency`
* `MaxDevices`
* `MaxSpeed`
* `Description`
* `Features`
* `IsActive`
* `DisplayOrder`
* `CreatedAt`
* `UpdatedAt`

## Связи между сущностями

```text
User (1) ──< Subscription (N)
User (1) ──< VPNConfig (N)
User (1) ──< Payment (N)
User (1) ──< TrafficStat (N)

Subscription (1) ──< VPNConfig (N)
Subscription (1) ──< Payment (N)

VPNServer (1) ──< VPNConfig (N)

VPNConfig (1) ──< TrafficStat (N)
```

## Индексы

В моделях проекта определены, в частности, следующие индексы:

* уникальный индекс `users.telegram_id`;
* индекс `subscriptions.user_id`;
* индекс `subscriptions.is_active`;
* индекс `vpn_configs.uuid`;
* индексы `vpn_configs.user_id`, `vpn_configs.subscription_id`, `vpn_configs.server_id`;
* индекс `payments.user_id`;
* индекс `payments.subscription_id`;
* индекс `payments.status`;
* индекс `vpn_servers.name`;
* индекс `vpn_servers.is_active`;
* составной индекс `traffic_stats(user_id, date)`.

## Репозитории

Слой репозиториев расположен в `internal/database/repository.go` и предназначен для инкапсуляции операций чтения и записи данных.

Назначение слоя репозиториев:

* изолировать бизнес-логику от деталей GORM;
* централизовать типовые операции с моделями;
* упростить unit-тестирование.

## Подключение к базе данных

Базовые параметры подключения задаются через `configs/.env`:

```env
DB_HOST=localhost
DB_PORT=5432
DB_USER=spiritdb
DB_PASSWORD=your_secure_password_here
DB_NAME=spiritdb
```

## Миграции

В текущей структуре проекта отдельная CLI-команда миграции отсутствует. Инициализация схемы выполняется через слой `internal/database` при вызове соответствующей логики приложения.

При использовании `AutoMigrate` в GORM следует учитывать следующее:

* таблицы создаются, если отсутствуют;
* недостающие столбцы могут быть добавлены;
* индексы могут быть созданы;
* удаление столбцов автоматически не выполняется.

## Тестирование слоя данных

Для тестирования репозиториев используются unit-тесты и мокирование SQL-запросов.

Основные инструменты:

* `github.com/stretchr/testify`
* `github.com/DATA-DOG/go-sqlmock`

Запуск тестов:

```bash
go test ./internal/database -v
```

## Практические рекомендации

* не хранить секреты БД в репозитории;
* использовать отдельную конфигурацию для production и local development;
* покрывать репозитории unit-тестами;
* поддерживать согласованность моделей, документации и конфигурации окружения.
