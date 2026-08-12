# SpiritVPN backend

Control plane доступа к VPN-флотам. Хранит желаемое состояние в PostgreSQL и
доводит его до node-agent'ов, управляющих Xray на нодах. Go, gRPC, PostgreSQL 14.

## Назначение

```text
Customer Service ────gRPC──┐
                           ├──> backend ──> PostgreSQL
infrastructure pipeline ───┘        │
                                    └──gRPC + mTLS──> node-agent ──> Xray API
```

Customer Service сообщает, кому и до какого момента положен доступ.
Infrastructure pipeline — из каких нод и связей состоит флот. Backend выпускает
индивидуальные credentials, отдаёт Customer Service готовые VLESS-ссылки и держит
на нодах тех пользователей, которым доступ положен сейчас.

Backend владеет customer access, `client_uuid`, accounting ID и желаемым составом
пользователей ноды. Вне его зоны:

* Xray — backend не обращается к нему напрямую, runtime users меняет только
  node-agent через loopback API;
* inbound, outbound, routing rules, транспорт между нодами и инфраструктурные
  credentials — infrastructure;
* подписки, billing, платежи — Customer Service;
* ручное управление runtime state: операторского API в v1 нет.

Node-agent разрабатывается в отдельном репозитории. Здесь лежит только
вендоренная копия контракта — `proto/spiritvpn/nodeagent/v1/node_agent.proto`.

## Внешний API

Backend принимает три gRPC-метода и сам вызывает пять у агентов. Всё под mTLS.

Входящие — два сервиса:

| сервис | методы | вызывает |
|---|---|---|
| `CustomerAccessService` | `ApplyCustomerAccess`, `GetCustomerAccessLinks` | Customer Service |
| `ManifestService` | `ApplyFleetManifest` | infrastructure pipeline |

Исходящие — `NodeAgentService` на каждой ноде:

| метод | кто вызывает | зачем |
|---|---|---|
| `EnsureUserPresent` | `dispatch` | завести пользователя на ноде |
| `EnsureUserAbsent` | `dispatch` | убрать пользователя с ноды |
| `GetNodeState` | `usage`, `reconcile` | забрать накопленный трафик и фактический список пользователей |
| `ReconcileUsers` | `reconcile` | заменить состав пользователей ноды целиком |
| `Health` | — | в v1 не используется: готовность ноды видна по исходу остальных вызовов |

На служебном HTTP-порту — `/health/live`, `/health/ready` и `/metrics`.
Аутентификации у них нет, порт наружу не публикуется: `/metrics` раскрывает размер
флота, объём трафика и состав очередей. HTTP-API для Customer Service нет.

### ApplyCustomerAccess

Единственная команда Customer Service: создание, продление и смена квоты — один и тот же
вызов.

```protobuf
message ApplyCustomerAccessRequest {
  string customer_id          = 1;  // непрозрачная строка Customer Service, 1..256 байт
  int64  vpn_fleet_id         = 2;  // целевой флот, > 0
  uint64 usage_quota_bytes    = 3;  // квота на каждую ноду отдельно, в байтах, > 0
  int64  expires_at_epoch_sec = 4;  // абсолютный момент окончания (UTC, секунды)
  uint64 command_number       = 5;  // монотонный счётчик команд Customer Service, > 0
}

message ApplyCustomerAccessResponse {}
```

* `command_number` даёт идемпотентность и порядок: команда с номером не больше
  сохранённого поглощается без последствий, ответ OK;
* `expires_at_epoch_sec` обязан расти; меньшее значение — отказ;
* квота применяется к каждой ноде отдельно; Customer Service переводит гигабайты в байты
  до вызова;
* пустой ответ означает «зафиксировано в БД». Доставка на ноды асинхронна.

### GetCustomerAccessLinks

Все действующие access customer сразу, без пагинации; retired исключены.

```protobuf
message GetCustomerAccessLinksRequest {
  string customer_id = 1;
}

message CustomerAccessLink {
  AccessKind      kind  = 1;  // ACCESS_KIND_FREEDOM | ACCESS_KIND_BRIDGE
  AccessLinkState state = 2;  // PENDING | READY | BLOCKED | FAILED

  // Только для BLOCKED: TIME_EXPIRED или TRAFFIC_QUOTA_EXHAUSTED.
  // Если сработало и то и другое — возвращается TIME_EXPIRED.
  optional AccessBlockReason block_reason = 3;

  // Только для READY.
  optional string uri = 4;
}

message GetCustomerAccessLinksResponse {
  repeated CustomerAccessLink links = 1;
}
```

Порядок ссылок в ответе стабилен: сортировка по `(kind, logical_target_key,
access_id)`. Сама ссылка нигде не хранится — собирается на лету из
расшифрованного `client_uuid` и текущего манифеста, поэтому ответ не кешируется и
не логируется.

### ApplyFleetManifest

Полный снимок топологии, применяется атомарно. Частичных манифестов не бывает.

```protobuf
message ApplyFleetManifestRequest {
  uint32 schema_version = 1;      // в v1 всегда 1
  uint64 revision       = 2;      // глобальная, строго возрастающая
  bool   allow_destructive = 3;   // разрешить удаление нод, membership и связей
  repeated ManifestNode  nodes  = 4;  // полный инвентарь нод снимка
  repeated ManifestFleet fleets = 5;  // полный инвентарь флотов снимка
}

message ManifestNode {
  string           node_id      = 1;
  NodeAgentConfig  agent        = 2;  // endpoint, TLS SNI, ожидаемая identity сертификата
  NodePublicConfig public       = 3;  // REALITY/VLESS-параметры, попадают в ссылку
  string           display_name = 4;  // человекочитаемое имя во фрагменте ссылки
}

message ManifestFleet {
  int64  vpn_fleet_id = 1;
  repeated string         node_ids = 2;  // участники флота
  repeated ManifestBridge bridges  = 3;  // направленные связи внутри флота
}

message ManifestBridge {
  string routing_key   = 1;  // стабильная идентичность связи
  string entry_node_id = 2;  // сюда ставится VLESS-пользователь customer
  string exit_node_id  = 3;  // нода выхода
  string egress_tag    = 4;  // тег Xray outbound на входной ноде; уйдёт агенту как egress_key
  string display_name  = 5;
}

message ApplyFleetManifestResponse {
  uint64              applied_revision = 1;  // ставшая текущей revision
  ManifestApplyResult result           = 2;  // APPLIED | IDEMPOTENT
}
```

* повтор той же `revision` с тем же содержимым идемпотентен и отвечает
  `IDEMPOTENT`; та же `revision` с другим содержимым и любая более старая
  `revision` отклоняются;
* `allow_destructive` действует только на этот запрос и не переносится на
  следующие. Удалить принятый ранее флот он не разрешает никогда;
* набор обязан быть полным: каждый принятый ранее флот присутствует в каждом
  следующем снимке;
* ограничения, которые protobuf не выражает — диапазоны, регулярные выражения,
  уникальность, ссылочная целостность, — backend проверяет до проекции;
* разница с предыдущим снимком раскладывается в изменения доступов всех customer
  флота отдельной джобой, уже после коммита.

## Воркеры

Семь видов, все в одном процессе с gRPC-сервером:

| воркер | что делает |
|---|---|
| `materialize` | раскладывает принятый манифест по customer: заводит новые access, ретайрит исчезнувшие цели |
| `dispatch` | доставляет агентам добавление и удаление пользователей, пишет исход, повторяет с backoff |
| `expiry` | прекращает доступ, когда вышел срок |
| `usage` | забирает у агентов накопленный трафик и начисляет в текущий период квоты |
| `reconcile` | сверяет фактический инвентарь Xray с желаемым и чинит расхождения |
| `prune-usage-dedup` | чистит реестр дедупликации usage-батчей |
| `stats` | снимает состояние БД для метрик |

`dispatch` и `usage` работают в восемь горутин каждый, остальные — в одну. Предел
«одна операция на ноду одновременно» держится в SQL, независимо от числа горутин.

Экземпляр backend один. Воркер, взяв задачу, помечает её в БД занятой собой на
ограниченное время; если процесс упал, срок истекает и задачу подбирает
следующий. Это защита от перезапуска и rolling deploy, а не механизм
масштабирования.

Схему накатывает команда `migrate` — шаг деплоя перед rollout.

## Доменная модель

Здесь — сущности, которыми оперирует код: customer и его срок, флот и его ноды,
доступ к отдельной ноде, период учёта квоты, операция для агента. Понадобится,
чтобы читать `internal/domain` и SQL-запросы: имена там те же, что в этом
разделе.

Всё состояние лежит в PostgreSQL; в памяти процесса ничего не хранится. Схема —
15 таблиц; их состав и колонки разобраны в [docs/DATABASE.md](docs/DATABASE.md).

### Customer и entitlement

`customer_id` — непрозрачная строка 1–256 байт из внешней системы. Backend её не
разбирает и не помещает ни в Xray, ни в логи, ни в метрики. Один customer
привязан к одному флоту, сменить флот в v1 нельзя.

Состояние customer — одна строка `customer_entitlements`: его флот, `expires_at` и
номер последней применённой команды. Все изменения сериализуются блокировкой этой
строки: конкурентные команды по одному customer выстраиваются в очередь, по разным
— идут параллельно.

### Флот, ноды и связи

Флот, его ноды и направленные BRIDGE → EXIT связи — проекция infrastructure
manifest. Идентичность ноды — `node_id`, идентичность связи — `routing_key`.
IP-адрес, домен, endpoint агента и физический сервер идентичностью не являются.

### Access

Действующему customer выпускается по одному доступу на каждую логическую цель
флота:

* `FREEDOM` — на каждую ноду, выход локальный;
* `BRIDGE` — на каждую связь, вход на `entry_node_id`, выход через её
  `egress_key`.

```text
link_count = fleet_node_count + bridge_relation_count
```

Флот с нодами `NL-1`, `DE-1` и связью `NL-1 → DE-1` даёт customer три access:
`NL-1 → Freedom`, `DE-1 → Freedom`, `NL-1 → DE-1`. На одной ноде у customer один
FREEDOM access и сколько угодно BRIDGE — по одному на каждую связь, где эта нода
входная. На EXIT-ноде credential customer не ставится.

Логическую цель обозначает `logical_target_key`: `node_id` для FREEDOM,
`routing_key` для BRIDGE. Он стабилен поверх поколений и участвует в уникальности
доступа. У каждого access — своя пара credentials.

`client_uuid` — учётные данные VLESS, по которым клиент проходит аутентификацию.
Хранится зашифрованным (AES-256-GCM), расшифровывается на лету.

`accounting_id` — псевдоним вида `u.` + 20 символов base32 из CSPRNG. Агент
подставляет его в поле `email` пользователя Xray, и оно работает сразу на две
вещи:

* **учёт** — Xray считает трафик по `email`, так что расход каждого access виден
  отдельно и попадает в квоту нужного customer;
* **маршрутизация** — для BRIDGE агент ставит правило `user:[accounting_id] →
  outbound`, отправляющее трафик этого пользователя в exit-outbound из
  `egress_key`. Пустой `egress_key` означает локальный выход, правило не
  ставится.

Отсюда требование глобальной уникальности `accounting_id`: совпадение перемешало
бы и счётчики, и маршруты двух customer. Префикс `u.` отличает пользователей
backend от инфраструктурных.

#### Поколения

Нода может исчезнуть из манифеста и вернуться — например, её вывели из
эксплуатации, а через месяц подняли заново под тем же `node_id`. Backend считает
это разными доступами, а не одним прерванным.

Исчезнувшая цель переводит свой access в retired: он больше не выдаётся и не
восстанавливается. Когда та же `logical_target_key` появляется снова, заводится
новый access с новыми `client_uuid` и `accounting_id`, а `generation` у него —
на единицу больше самого большого из прежних для этой цели. У первого access
цели `generation = 1`.

```text
NL-1 добавлена в манифест      → access #1, generation 1
NL-1 удалена из манифеста      → access #1 retired
NL-1 добавлена снова           → access #2, generation 2, новый client_uuid
```

Старые ссылки после возврата ноды не оживают: у вернувшейся ноды другой
`client_uuid`, и прежняя VLESS-ссылка на ней не аутентифицируется. Retired-строки
остаются в БД — они и хранят, до какого номера поколение уже доросло. Ручного
enable/disable в v1 нет, ничего физически не удаляется.

### Срок и квота

Access на входной ноде действует, пока выполнено:

```text
current_time < expires_at
AND node_quota_period_usage_bytes < usage_quota_bytes
```

Срок общий на всего customer. Квота применяется к каждой ноде независимо: трафик
всех FREEDOM и BRIDGE access customer на одной ноде идёт в один счётчик,
исчерпание на одной ноде не влияет на доступ на других.

`usage_quota_bytes` лежит не в entitlement, а в строке периода учёта — вместе с
потраченным в нём трафиком. Renewal закрывает текущий период и открывает
следующий: новая квота, обнулённый расход, прежние credentials. Текущий период
всегда один — тот, у которого `closed_at IS NULL`.

### Доставка на ноду

Желаемое состояние и достигнутое разнесены по разным полям:

* `desired_state` — `PRESENT` или `ABSENT`. Меняется решением домена: команда
  Customer Service, новый манифест, истечение срока, исчерпание квоты;
* `desired_version` — растёт на единицу с каждым изменением, различает версии
  желания;
* `apply_state` — `PENDING`, `APPLIED`, `RETRYING`, `FAILED`. Провал доставки
  желаемого состояния не меняет.

Изменение желаемого состояния кладёт операцию в очередь в той же транзакции.
`dispatch` берёт операцию, вызывает у агента `EnsureUserPresent` или
`EnsureUserAbsent` и записывает исход.

У операции стабильный `operation_id`, сохраняющийся между попытками — по нему
агент отсеивает дубли. Пара `(access_id, desired_version)` уникальна, поэтому
повторное планирование того же изменения второй операции не создаёт, а обогнанная
более свежей версией становится `SUPERSEDED` и не поедет никогда. Колонки целиком
— в [docs/DATABASE.md](docs/DATABASE.md).

`client_uuid` и `egress_key` в очереди не лежат: они собираются из строки access
перед самым вызовом.

`reconcile` периодически запрашивает у агента фактический список пользователей
ноды и сличает с желаемым. Расхождение чинится полным списком целиком — нода
могла потерять состояние вся сразу: переустановка, откат образа, пустой конфиг
Xray.

Трафик backend забирает сам: `usage` вызывает `GetNodeState`, помнит по каждой
ноде позицию, до которой дочитал, и начисляет прочитанное в текущий период.

Время доменных решений — `now()` PostgreSQL, момент начала транзакции. Истечение,
границы периодов и backoff считаются в SQL.

## Структура репозитория

```text
cmd/
  spiritvpnd/          процесс backend: gRPC-сервер, HTTP health/metrics и все фоновые воркеры
  migrate/             команда миграций схемы: up, down, down-all, version, force
internal/
  domain/              чистые правила: план access, поколения, квоты, истечение, планы манифеста,
                       VLESS-ссылки. Детерминирован: без часов, без генерации ID, без БД
  app/                 use case'ы (apply, links, manifest, materialize, dispatch, expiry, usage,
                       reconcile, inventory, prune, stats) и порты к инфраструктуре
  config/              разбор переменных SPIRIT_* из окружения; у секретов нет умолчаний,
                       в логи конфигурация попадает без DSN и без ключа шифрования
  crypto/              AES-256-GCM для client_uuid, генерация accounting_id и идентификаторов
  grpcsvc/             входящие gRPC-адаптеры: mTLS-аутентификация, маппинг ошибок, request-id
  nodeagent/           исходящий gRPC-клиент к агентам: mTLS, вызовы desired/reconcile/usage
  postgres/            адаптеры портов app поверх pgx: транзакции, блокировки, аренда задач
    queries/           SQL-запросы — источник для sqlc
    db/                код, сгенерированный sqlc; править руками нельзя
  migrations/          versioned SQL схемы и мигратор поверх golang-migrate
  metrics/             реестр Prometheus: декораторы портов app и снимок состояния БД
  gen/                 protobuf- и gRPC-код, сгенерированный buf; править руками нельзя
proto/spiritvpn/
  customer/v1/         Customer API: ApplyCustomerAccess, GetCustomerAccessLinks
  manifest/v1/         Manifest API: ApplyFleetManifest
  nodeagent/v1/        вендоренная копия контракта backend ↔ node-agent, владелец — infra
docs/                  исходники сайта документации (mkdocs-material)
.github/workflows/     CI: сборка и тесты, покрытие, changelog, публикация docs, PR-автоматика
Makefile               сборка, генерация, dev-база, dev-сертификаты, прогоны тестов
sqlc.yaml              генерация db/ из queries/
buf.yaml, buf.gen.yaml генерация gen/ из proto/
docker-compose.dev.yml PostgreSQL для разработки и интеграционных тестов (порт 5433, tmpfs)
docker-compose.yml     локальное превью сайта документации на порту 8081
```

Зависимости идут в одну сторону: `grpcsvc` и `cmd` → `app` → `domain`.
`postgres`, `crypto`, `nodeagent` и `metrics` подключаются к `app` через его
порты. `domain` не знает про PostgreSQL, gRPC и wire-схему манифеста, поэтому его
тесты — таблицы «вход → ожидаемый план», без моков.

## Требования

* Go 1.26.0 — версия зафиксирована в `go.mod`;
* PostgreSQL 14 — та же мажорная, что в CI;
* Docker с плагином Compose — для локальной базы и превью документации;
* openssl — для выпуска локальных сертификатов mTLS;
* make.

`buf`, `sqlc` и плагины `protoc-gen-*` ставить отдельно не нужно: они запиннены
как tool-зависимости в `go.mod` и вызываются через `go tool`, поэтому версия у
всех одна.

## Быстрый старт

```bash
make dev          # PostgreSQL на 5433 в tmpfs, схема накачена
make dev-certs    # локальный CA, сертификаты сервера и клиента product-svc
```

Insecure-режима у сервера нет: mTLS обязателен и локально тоже. `make dev-certs`
кладёт в `dev/certs/` CA, серверную пару и клиентскую пару с DNS SAN
`product-svc` — именно это имя backend и проверяет как идентичность клиента.

Дальше процессу нужны переменные окружения. Минимальный набор для запуска на
локальных сертификатах:

```bash
export SPIRIT_DATABASE_URL='postgres://spiritdb:spiritdb@localhost:5433/spiritdb?sslmode=disable'
export SPIRIT_CLIENT_UUID_KEY="dev-1:$(openssl rand -base64 32)"
export SPIRIT_GRPC_TLS_CERT_FILE=dev/certs/server.crt
export SPIRIT_GRPC_TLS_KEY_FILE=dev/certs/server.key
export SPIRIT_GRPC_TLS_CLIENT_CA_FILE=dev/certs/ca.crt
export SPIRIT_AGENT_TLS_CERT_FILE=dev/certs/product-svc.crt
export SPIRIT_AGENT_TLS_KEY_FILE=dev/certs/product-svc.key
export SPIRIT_AGENT_TLS_CA_FILE=dev/certs/ca.crt
export SPIRIT_ROLE_CUSTOMER_ACCESS_WRITER=product-svc
export SPIRIT_ROLE_CUSTOMER_ACCESS_READER=product-svc
export SPIRIT_ROLE_MANIFEST_WRITER=product-svc

go run ./cmd/spiritvpnd
```

Процесс поднимет gRPC на `:8443`, служебный HTTP на `:8080` и запустит все
воркеры. Готовность — `curl localhost:8080/health/ready`.

Клиентская пара к агентам здесь взята та же, что и product-svc, только чтобы
процесс стартовал: настоящих агентов локально нет, и до них никто не дойдёт. В
продакшене это разные сертификаты.

## Конфигурация

Только переменные окружения, все с префиксом `SPIRIT_`. Аргументами командной
строки секреты не передаются: они видны в `ps` и оседают в истории оболочки.
Отсутствие обязательной переменной валит старт — молча подставленный дефолт
секрета означал бы работающий процесс с неверными учётными данными. Ошибки
собираются все разом, а не по одной на перезапуск.

| переменная | по умолчанию | назначение |
|---|---|---|
| `SPIRIT_DATABASE_URL` | — | строка подключения к PostgreSQL; секрет целиком |
| `SPIRIT_DB_MAX_CONNS` | `10` | верхняя граница пула соединений |
| `SPIRIT_LOG_LEVEL` | `info` | уровень логирования |
| `SPIRIT_GRPC_LISTEN` | `:8443` | адрес gRPC-поверхности |
| `SPIRIT_HTTP_LISTEN` | `:8080` | адрес служебного HTTP |
| `SPIRIT_GRPC_TLS_CERT_FILE` | — | серверный сертификат |
| `SPIRIT_GRPC_TLS_KEY_FILE` | — | серверный приватный ключ |
| `SPIRIT_GRPC_TLS_CLIENT_CA_FILE` | — | CA, которым подписаны сертификаты клиентов |
| `SPIRIT_AGENT_TLS_CERT_FILE` | — | клиентский сертификат для походов к агентам |
| `SPIRIT_AGENT_TLS_KEY_FILE` | — | клиентский приватный ключ |
| `SPIRIT_AGENT_TLS_CA_FILE` | — | CA инфраструктуры, которым подписаны сертификаты агентов |
| `SPIRIT_ROLE_CUSTOMER_ACCESS_WRITER` | — | идентичности, которым разрешён `ApplyCustomerAccess` |
| `SPIRIT_ROLE_CUSTOMER_ACCESS_READER` | — | идентичности, которым разрешён `GetCustomerAccessLinks` |
| `SPIRIT_ROLE_MANIFEST_WRITER` | — | идентичности, которым разрешён `ApplyFleetManifest` |
| `SPIRIT_CLIENT_UUID_KEY` | — | ключ шифрования `client_uuid`, формат `<key_id>:<base64 32 байта>` |
| `SPIRIT_CLIENT_UUID_KEY_FILE` | — | путь к файлу с тем же значением; имеет приоритет над переменной выше |

Две пары TLS-сертификатов разные намеренно: в одном случае backend принимает
вызывающего и предъявляет себя ему, в другом сам приходит клиентом к агенту.
Общий сертификат означал бы, что компрометация одной роли компрометирует и
вторую.

Роли независимы: writer не подразумевает reader, потому что чтение отдаёт
VLESS-ссылку вместе с `client_uuid`, то есть сами credentials. Сервису, которому
нужно и то и другое, идентичность прописывается в оба списка. Списки —
через запятую. Если все три пусты, старт падает: такой процесс отвечал бы
`PERMISSION_DENIED` на каждый вызов, и заметить это можно было бы только по
жалобам вызывающего.

## Разработка

```bash
make help          # список целей
make build         # бинарники в bin/
make lint          # golangci-lint
make hooks         # git-хуки через pre-commit
```

Генерация — после правки исходников:

```bash
make proto         # internal/gen из proto/ (buf, managed mode)
make sqlc          # internal/postgres/db из internal/postgres/queries/
make sqlc-vet      # проверить запросы по схеме без генерации
```

`internal/gen` и `internal/postgres/db` руками не правятся — только
перегенерацией. sqlc читает схему из `internal/migrations`, то есть источник
истины для базы и для генерации один.

Миграции: `make migrate-up`, `make migrate-down`, `make migrate-version`.
Отдельная команда, а не автомиграция при старте — это шаг деплоя перед rollout.

Превью документации: `make docs-serve`, дальше http://localhost:8081.

## Тестирование

```bash
make test-unit           # быстрые тесты, без базы
make dev-db              # база для интеграционных
make test-integration
make dev-db-down
make test-coverage       # всё под -race с профилем покрытия
```

Интеграционные тесты идут против настоящего PostgreSQL той же мажорной версии,
что в CI. Они делают `TRUNCATE` всех таблиц перед каждым тестом, поэтому база
поднимается на отдельном порту 5433 и с данными в tmpfs. Отбираются по имени:
`make test-integration` запускает `-run Integration`, так что имя такого теста
обязано начинаться с `TestIntegration`.

Порог покрытия в CI — 80%.

## Документация

`docs/` — исходники сайта на mkdocs-material, публикуется на GitHub Pages при
push в `main`. Подробности схемы и работы с ней — в
[docs/DATABASE.md](docs/DATABASE.md).
