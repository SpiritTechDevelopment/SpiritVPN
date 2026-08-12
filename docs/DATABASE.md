# База данных

PostgreSQL 14. Всё состояние backend живёт здесь: в памяти процесса не хранится
ничего, что нельзя потерять при перезапуске.

## Как устроена работа со схемой

ORM нет. Схема задаётся versioned SQL-миграциями, запросы пишутся вручную, а код
доступа к ним генерируется.

* **Миграции** — `internal/migrations`, поверх golang-migrate. Файлы встроены в
  бинарник через `embed`, накатывает их отдельная команда `migrate` — шаг деплоя
  перед rollout. Приложение схему не меняет и при старте её не трогает.
* **Запросы** — `internal/postgres/queries/*.sql`. По ним sqlc генерирует
  `internal/postgres/db`; этот каталог правится только перегенерацией
  (`go tool sqlc generate`).
* **Драйвер** — pgx. Транзакциями владеют адаптеры в `internal/postgres`, порядок
  шагов внутри транзакции задают use case'ы в `internal/app`.

## Соглашения

* Все временные метки — `timestamptz` в UTC.
* Значения `uint64` с провода — счётчики байтов, `command_number`, порядковые
  номера спула, байты квоты — хранятся как `numeric(20,0)`. Внутренние счётчики
  backend (`int64`) — как `bigint`.
* Перечислимые значения — `text` с `CHECK`, а не нативный `ENUM`: набор значений
  должен эволюционировать обычными миграциями.
* `ON DELETE CASCADE` не используется нигде. История не удаляется — строки
  помечаются (`retired_at`, `closed_at`, `exhausted_at`), а внешние ключи нужны
  только для ссылочной целостности.

## Состав схемы

15 таблиц, пятью группами.

### Топология

Проекция infrastructure manifest. Authority снаружи: backend эти строки не
придумывает, а принимает.

| таблица | назначение |
|---|---|
| `manifest_revisions` | журнал принятых снимков; `revision` глобальна и строго возрастает, `digest` отличает идемпотентный повтор от конфликтующего |
| `vpn_fleets` | флоты |
| `vpn_nodes` | ноды: endpoint агента, публичные REALITY/VLESS-параметры |
| `vpn_fleet_nodes` | состав флота |
| `vpn_bridge_routes` | направленные связи BRIDGE → EXIT |

### Доступ

Authority у backend.

| таблица | назначение |
|---|---|
| `customer_entitlements` | одна строка на customer: флот и срок |
| `quota_periods` | периоды учёта квоты |
| `node_quota_usage` | расход customer на каждой ноде внутри периода |
| `vpn_accesses` | одна строка на доступ: credentials и состояние |

### Доставка

| таблица | назначение |
|---|---|
| `agent_operations` | очередь команд агентам, она же журнал исполнения |
| `manifest_materialization_jobs` | разбор принятого манифеста по всем customer, пакетами после коммита |

### Учёт трафика

| таблица | назначение |
|---|---|
| `node_usage_cursors` | позиция чтения по каждой ноде; она же lease-таблица pull-воркера |
| `traffic_usage_items_processed` | реестр уже начисленного: пока строка жива, повторное начисление того же item невозможно |
| `traffic_batch_quarantine` | items, которые не удалось разобрать, — чтобы один плохой не блокировал батч |

### Аудит

| таблица | назначение |
|---|---|
| `audit_events` | append-only журнал значимых действий |

## Ключевые таблицы

### customer_entitlements

Корневая строка customer и точка сериализации: все изменения его состояния идут
под `SELECT ... FOR UPDATE` этой строки.

| колонка | тип | что означает |
|---|---|---|
| `customer_id` | `text` PK | непрозрачная строка Customer Service, 1–256 байт |
| `vpn_fleet_id` | `bigint` | флот customer; сменить его в v1 нельзя |
| `expires_at` | `timestamptz` | момент окончания доступа, общий на всего customer |
| `desired_version` | `bigint` | внутренний счётчик изменений состояния; наружу не выдаётся |
| `last_command_number` | `numeric(20,0)` | номер последней применённой команды; команда с номером не больше него игнорируется |
| `created_at`, `updated_at` | `timestamptz` | |

### quota_periods

Квота хранится здесь, а не в entitlement: она осмысленна только вместе с
потраченным в периоде трафиком. Renewal закрывает текущий период и открывает
следующий.

| колонка | тип | что означает |
|---|---|---|
| `quota_period_id` | `uuid` PK | |
| `customer_id` | `text` | владелец периода |
| `started_at`, `closed_at` | `timestamptz` | границы; `closed_at IS NULL` ровно у одного, текущего |
| `usage_quota_bytes` | `numeric(20,0)` | квота на каждую ноду отдельно, `> 0` |

Периоды — непересекающиеся полуоткрытые интервалы `[started_at, closed_at)`. При
renewal `closed_at` старого совпадает со `started_at` нового с точностью до
значения, иначе дельта трафика не попала бы ни в один период.

### node_quota_usage

| колонка | тип | что означает |
|---|---|---|
| `quota_period_id`, `node_id` | | составной первичный ключ |
| `uplink_bytes`, `downlink_bytes` | `numeric(20,0)` | накопленный трафик |
| `total_bytes` | `numeric(20,0)` | генерируемая колонка, `uplink + downlink`, хранится (STORED) |
| `exhausted_at` | `timestamptz` | момент исчерпания квоты на этой ноде; он же признак блокировки — отдельной таблицы блокировок нет |
| `updated_at` | `timestamptz` | |

Трафик всех FREEDOM и BRIDGE access customer на одной ноде идёт в одну строку.
Исчерпание на одной ноде не влияет на доступ на других.

### vpn_accesses

| колонка | тип | что означает |
|---|---|---|
| `access_id` | `uuid` PK | |
| `customer_id` | `text` | владелец доступа |
| `kind` | `text` | `FREEDOM` или `BRIDGE` |
| `logical_target_key` | `text` | логическая цель: `node_id` для FREEDOM, `routing_key` для BRIDGE |
| `generation` | `integer` | какой это по счёту доступ к этой цели, с единицы |
| `entry_node_id` | `text` | нода, на которой заводится пользователь |
| `egress_key` | `text` | пустая строка для FREEDOM, тег exit-outbound для BRIDGE; передаётся агенту дословно |
| `accounting_id` | `text` UNIQUE | псевдоним, уходит в Xray как `email` |
| `encrypted_client_uuid` | `bytea` | учётные данные VLESS, AES-256-GCM |
| `encryption_key_id` | `text` | каким ключом зашифровано |
| `desired_state` | `text` | `PRESENT` или `ABSENT` |
| `apply_state` | `text` | `PENDING`, `APPLIED`, `RETRYING`, `FAILED` |
| `desired_version` | `bigint` | версия желаемого состояния |
| `retired_at` | `timestamptz` | момент исчезновения цели из манифеста; у действующих пусто |
| `created_at` | `timestamptz` | |

Уникальность — `(customer_id, kind, logical_target_key, generation)`. Строка не
удаляется никогда: retired-строки хранят, до какого номера доросло поколение
цели.

### agent_operations

Transactional outbox: операция появляется в той же транзакции, что и изменение
`desired_state`.

| колонка | тип | что означает |
|---|---|---|
| `operation_id` | `uuid` PK | сохраняется между попытками; по нему агент отсеивает дубли |
| `node_id`, `access_id` | | куда и про какой доступ |
| `operation_type` | `text` | `ENSURE_PRESENT` или `ENSURE_ABSENT` |
| `desired_version` | `bigint` | какую версию желания везёт операция |
| `status` | `text` | `PENDING`, `IN_FLIGHT`, `SUCCEEDED`, `RETRY_WAIT`, `FAILED_PERMANENT`, `SUPERSEDED` |
| `attempt_count`, `next_attempt_at` | | сколько было попыток и когда можно следующую |
| `lease_owner`, `lease_expires_at` | | кто взял операцию в работу и до какого момента |
| `last_error_code`, `last_error_message` | `text` | чем закончилась последняя попытка |
| `created_at`, `completed_at` | `timestamptz` | |

Уникальность — `(access_id, desired_version)`: повторное планирование того же
изменения второй операции не создаёт.

Полезной нагрузки здесь нет. `client_uuid` и `egress_key` собираются из строки
access перед самым вызовом агента, иначе застрявшая операция везла бы данные,
устаревшие к моменту доставки.

## Инварианты, которые держит сама база

Часть правил вынесена в схему, потому что проверка в коде читает
закоммиченный снимок и гонку не ловит.

* **Одна операция в полёте на ноду** — `agent_operations_single_in_flight_per_node`,
  частичный уникальный индекс по `node_id` при `status = 'IN_FLIGHT'`. Проигранная
  гонка опознаётся по имени constraint'а.
* **Один открытый период квоты на customer** — `quota_periods_one_open_per_customer`,
  частичный уникальный индекс при `closed_at IS NULL`.
* **Один действующий access на логическую цель** — `vpn_accesses_current_target`,
  частичный уникальный индекс при `retired_at IS NULL`. Он же путь поиска для
  Customer API.
* **Глобальная уникальность `accounting_id`** — `UNIQUE` на колонке. Коллизия
  обязана уронить транзакцию, а не привести к повторной генерации.

## Локальная база

```bash
make dev           # PostgreSQL на порту 5433 (tmpfs) и накатить схему
make dev-db-down   # остановить и удалить данные
```

Порт 5433 и tmpfs выбраны намеренно: интеграционные тесты делают `TRUNCATE` всех
таблиц перед каждым тестом, и эта база должна быть заведомо одноразовой.
