# Архитектура

Один процесс, `spiritvpnd`, и одна база. Процесс принимает команды по gRPC, ведёт
состояние в PostgreSQL и доставляет его агентам нод. В памяти процесса не живёт
ничего, что нельзя потерять при перезапуске.

Единица состояния это access: доступ одного customer к одной цели, то есть к ноде
флота (`FREEDOM`) либо к связи между двумя нодами (`BRIDGE`). У access есть
desired state, `PRESENT` или `ABSENT`, отвечающий на вопрос, должен ли юзер
присутствовать на входной ноде. Backend выводит desired state из четырёх
источников: команд product-сервиса, манифеста, расхода квоты и срока действия.
Доставка на ноду асинхронна и идёт через таблицу `agent_operations`.

Наружу смотрят два gRPC-сервиса под mTLS: `CustomerAccessService` для
product-сервиса и `ManifestService` для infrastructure pipeline. На отдельном
служебном порту живут `/health/live`, `/health/ready` и `/metrics`.

## Где что лежит

| каталог | что внутри |
|---|---|
| `cmd/spiritvpnd` | composition root, сборка gRPC и HTTP, цикл воркеров, все интервалы и TTL |
| `cmd/migrate` | накат миграций, отдельный шаг деплоя |
| `internal/domain` | правила без ввода-вывода: `PlanApply`, `PlanMaterialize`, `PlanUsageGroup`, `PlanExpiry`, `PlanManifest`, `PlanOperationResult` |
| `internal/app` | use case'ы, порты (`ports.go`, `repository.go`), сверка инвентаря (`inventory.go`), сборка VLESS URI (`vless.go`) |
| `internal/postgres` | адаптеры портов, по файлу на путь; `queries/*.sql` и сгенерированный `db/` |
| `internal/grpcsvc` | хендлеры, interceptor'ы, таблица ролей (`auth.go`), отображение ошибок (`errors.go`) |
| `internal/nodeagent` | клиент агентов: четыре RPC, mTLS, классификация исходов |
| `internal/crypto` | `client_uuid`, `accounting_id`, AES-GCM поверх ключа из файла |
| `internal/metrics` | реестр Prometheus, декораторы портов, снимок состояния БД |
| `internal/migrations` | `0001_baseline.up.sql`, встроен в бинарь через `embed` |

Читать с `cmd/spiritvpnd/main.go`: там видно, из чего собран процесс. Дальше по
интересующему пути, снизу вверх или сверху вниз по таблицам ниже.

## Общая форма пути

Одинакова у всех путей записи.

Хендлер в `grpcsvc` переводит protobuf в доменную команду и вызывает use case.
Своих правил у него нет.

Use case в `app` открывает транзакцию через порт и задаёт порядок шагов внутри неё.
Шаги перечислены методами интерфейса в `repository.go`, по одному на группу
блокируемых строк.

Домен получает готовый снимок и возвращает план изменений. Он ничего не пишет, не
читает часы и не генерирует идентификаторы: `access_id`, `accounting_id`,
`client_uuid` и `operation_id` добавляет use case из портов `IDs` и
`CredentialSealer`, время приходит из `SELECT now()` первым оператором транзакции.

Адаптер в `postgres` владеет транзакцией (`READ COMMITTED`, commit, rollback) и
применяет план одним методом `Write*`, внутри которого запись идёт в порядке
блокировок:

```
customer_entitlements → quota_periods → node_quota_usage
  → [traffic_usage_items_processed] → vpn_nodes → vpn_accesses → agent_operations
```

Сетевых вызовов агентам ни один из путей записи не делает. Он кладёт строки в
`agent_operations`, дальше работает диспетчер.

## Состояния

Три перечислимых колонки, которые встречаются на каждом пути.

`vpn_accesses.desired_state`, чего система хочет: `PRESENT` либо `ABSENT`. Меняется
только путями записи, и каждое изменение увеличивает `desired_version` этой строки.

`vpn_accesses.apply_state`, что известно про ноду. Это проекция исхода последней
операции на строку access.

| значение | что означает | откуда берётся |
|---|---|---|
| `PENDING` | доставка не подтверждена | вставка `PRESENT`-access и любая смена `desired_state` |
| `APPLIED` | агент подтвердил | успешная доставка либо приём полного набора; сразу при создании `ABSENT`-access и при ретайре access, чью ноду убрали из манифеста |
| `RETRYING` | попытка не удалась, повтор будет | запись ретраибельного исхода |
| `FAILED` | попытка не удалась терминально | запись permanent-исхода |

`agent_operations.status`, судьба одной доставки. Строки не удаляются, таблица
служит ещё и журналом исполнения.

| значение | что означает |
|---|---|
| `PENDING` | в очереди, к агенту не уходила |
| `IN_FLIGHT` | взята под lease: либо RPC идёт, либо воркер умер, не записав исход |
| `SUCCEEDED` | агент подтвердил, что на ноде состояние такое, как просили |
| `RETRY_WAIT` | попытка не удалась, следующая не раньше `next_attempt_at` |
| `FAILED_PERMANENT` | агент ответил так, что повтор не поможет. Само не разойдётся: нужна смена desired state либо полный набор от reconcile |
| `SUPERSEDED` | пока операция ждала, desired state сменился |

`SUPERSEDED` это не отказ доставки. Операцию обогнали: `desired_version` строки
access ушла вперёд той версии, под которую операция выпускалась, и отправлять её
уже незачем. Например, команда включила доступ, а через секунду истёк срок и
выключил его. Операция на включение становится `SUPERSEDED`, до ноды доезжает
только выключение.

Три статуса терминальны и получают `completed_at`: `SUCCEEDED`,
`FAILED_PERMANENT`, `SUPERSEDED`. `RETRY_WAIT` держит `next_attempt_at` и пустой
`completed_at`.

## Фоновые воркеры

Восемь воркеров, у всех одна форма: `ProcessNext(ctx) (progressed bool, err error)`.
Цикл в `cmd/spiritvpnd/worker.go` при `progressed` идёт на следующий шаг без паузы,
на пустом проходе спит `idle`, на ошибке пишет в лог и спит 15 секунд.

`progressed` означает «шаг сделал работу», а не «состояние изменилось».
Недоступная нода даёт `true`: повторять немедленно нечего, темп задаёт свой
интервал.

| воркер | горутин | idle | как берёт работу |
|---|---|---|---|
| `materialize` | 1 | 5 с | `ClaimMaterializationJob`, lease 60 с |
| `expiry` | 1 | 5 с | `LockNextDueExpiredCustomer`, `FOR UPDATE SKIP LOCKED` |
| `finalize-deletion` | 1 | 5 с | `LockNextDeletionCandidate`, `FOR UPDATE SKIP LOCKED` |
| `dispatch` | 8 | 1 с | `LeaseNextOperation`, lease 30 с |
| `usage` | 8 | 3 с | `ClaimUsageNode`, lease 5 мин, не чаще 15 с на ноду |
| `reconcile` | 1 | 15 с | `ClaimNodeForReconcile`, lease 5 мин, не чаще 5 мин на ноду |
| `prune-usage-dedup` | 1 | 5 мин | ничего не берёт, удаляет по возрасту |
| `stats` | 1 | 15 с | ничего не берёт, читает пять запросов |

Экземпляр процесса один. Lease нужен, чтобы после падения работу подобрал
следующий шаг, а не для горизонтального масштабирования.

## Команда product-сервиса

`ApplyCustomerAccess`: `customer_id`, `vpn_fleet_id`, `usage_quota_bytes`,
`expires_at_epoch_sec`, `command_number`.

До хендлера три interceptor'а, `cmd/spiritvpnd/grpc.go:71`: request_id в контекст,
запись в лог, проверка SAN клиентского сертификата по списку
`customer-access-writer`.

Хендлер `grpcsvc/customer_access.go:70` собирает `app.ApplyCustomerCommand`,
добавляя `peerIdentity(ctx)` и request_id для журнала. Ошибку отображает
`statusFromError`.

`app.ApplyCustomerAccess.Execute`, `app/apply_customer_access.go:59`:

| шаг | вызов | что делает |
|---|---|---|
| 1 | `domain.ValidateApplyCommand` | до транзакции |
| 2 | `tx.Now` | `SELECT now()` |
| 3 | `tx.LockEntitlement` | `LockCustomerEntitlement`, `FOR UPDATE`; `nil` = новый customer |
| 4 | `domain.ClassifyCommand` | меньший номер → stale, равный fingerprint → replay, равный номер с другим payload → conflict |
| 5 | `tx.FleetIsCurrent` | флот присутствует в последнем принятом манифесте |
| 6 | `LockOpenQuotaPeriod`, `LockNodeQuotaUsage`, `LoadTopology`, `LoadAccesses` | первые два `FOR UPDATE`, вторые два без locks |
| 7 | `PlanApply` → `WritePlan` → `AppendAudit` | |

`domain.PlanApply`, `domain/apply.go:110`, считает состояние квоты, каким оно
станет после этой транзакции, вызывает `PlanAccessSet` за недостающими access и
`PlanDesiredChanges` за пересчётом существующих. Возвращает `ApplyPlan` со
списками, отсортированными по `node_id` и `access_id`.

`uc.materialize`, `app/apply_customer_access.go:210`, выдаёт каждому создаваемому
access `NewAccessID`, `NewAccountingID`, `NewClientUUID` и `Seal`. Операция
создаётся только для `PRESENT`: новый `ABSENT`-access ничего на ноде не меняет.

`applyTx.WritePlan`, `postgres/apply.go`, шесть шагов:

* `writeEntitlement`: `InsertCustomerEntitlement`, `UpdateCustomerEntitlement`
  либо реактивация `DELETED` tombstone.
  `last_command_number` двигается всегда, включая пустой план;
* `writeQuotaPeriod`: при create и renewal `CloseOpenQuotaPeriod` и
  `InsertQuotaPeriod` одним timestamp, при смене лимита `UpdateQuotaPeriodQuota`;
* `writeNodeQuotaUsage`: `InsertNodeQuotaUsage` на все ноды флота нового периода,
  затем `SetNodeQuotaExhausted` по каждому пересчёту под новый лимит. Этот же
  запрос снимает отметку, записывая `NULL`: поднятый лимит разблокирует ноду;
* `writeTouchedNodes`: `LockNodesForUpdate` в порядке `node_id`, затем
  `BumpNodesDesiredRevision` по одному разу на ноду;
* `writeAccesses`: `InsertVpnAccess` и `UpdateAccessDesiredState`;
* `writeOperations`: `SupersedeStaleOperations` по каждому изменённому access,
  затем `InsertAgentOperation` в `PENDING`.

После commit юзера на ноде ещё нет. Пустой ответ означает, что desired state и
операции зафиксированы в базе.

## Административный lifecycle

`customer_entitlements.lifecycle_state` образует автомат
`ACTIVE ↔ BLOCKED → DELETING → DELETED`. Все три командных RPC используют одну
корневую row lock, один `last_command_number` и fingerprint, поэтому Apply и
Delete не могут обойти порядок друг друга.

`SetCustomerAccessState(BLOCKED)` и `DeleteCustomerAccess` строят чистый
`PlanForceAbsent`, повышают `desired_version`, supersede'ят старые ожидающие
операции и атомарно кладут `ENSURE_ABSENT` в outbox для нод актуального manifest.
В план входят также retired-поколения: на исчезнувших нодах они становятся
`APPLIED` без недоставляемой операции. Уже `ABSENT` access также получает новую
версию: на живой ноде административная команда подтверждает физическое
отсутствие, а не доверяет только сохранённому флагу.

Delete возвращает `PENDING`. `FinalizeCustomerDeletions` очищает одного
сошедшегося customer в явном FK-порядке и превращает корневую строку в tombstone.
В tombstone остаются ordering token и `customer_id`; следующий Apply с большим
номером атомарно создаёт новый quota period и новые credentials.

### Fence dispatcher/reconcile

Incremental mutating RPC и `ReconcileUsers` не выполняются одновременно на одной
ноде. `LeaseNextOperation` сначала блокирует строку `vpn_nodes` и проверяет
отсутствие reconcile lease; `ClaimNodeForReconcile` под той же row lock проверяет
отсутствие `IN_FLIGHT` operation. Если старый reconcile начался раньше Delete,
новая `ENSURE_ABSENT` выполнится строго после него и останется последней
физической операцией.

## Выдача ссылок

`GetCustomerAccessLinks`, роль `customer-access-reader`. Хендлер ставит в метадату
ответа `cache-control: no-store`.

`app.GetCustomerAccessLinks.Execute`, `app/get_customer_access_links.go:39`,
проверяет `customer_id` и делает один вызов `LoadCustomerLinks`. Row locks на этом
пути не берутся, записей нет.

`postgres/links.go:29` читает `GetCustomerLinksHeader` (время базы и `expires_at`)
и `ListCustomerAccessLinks`. Второй запрос отдаёт строки, отсортированные по
`(kind, logical_target_key, access_id)`; ретайрнутые access и цели, которых нет в
текущем манифесте, в выборку не попадают.

Тот же запрос читает лимит открытого `quota_periods` и `total_bytes` из
`node_quota_usage`. Строка расхода присоединяется по
`usage.node_id = vpn_accesses.entry_node_id`: у FREEDOM это сама нода, у BRIDGE —
entry-нода связи. Поэтому ссылки с общей entry-нодой получают одинаковые
`usage_quota_bytes` и `consumed_bytes`; трафик exit-ноды BRIDGE в эти поля не
подмешивается.

Состояние каждой ссылки выводит `domain.LinkStatusOf`, `domain/link.go`, из
`desired_state`, `apply_state`, `expires_at`, признака исчерпанной квоты и
пригодности `public_config` входной ноды. Состояние нигде не хранится и считается
на каждый ответ.

Словарь у ответа свой, не совпадающий с `apply_state`: `PENDING` (доставка ещё
идёт), `READY` (URI заполнена), `BLOCKED` с причиной `TIME_EXPIRED` либо
`TRAFFIC_QUOTA_EXHAUSTED`, `FAILED`.

`Sealer.Open` вызывается только на ветке `READY`, URI собирает
`app.BuildVLESSURI`, `app/vless.go`. Отказ расшифрования гасит эту одну ссылку до
`FAILED`, остальные в ответе остаются. Quota-поля переносятся в ответ до выбора
ветки состояния и поэтому присутствуют также у `PENDING`, `BLOCKED` и `FAILED`.

## Каталог нод до покупки

`ListAvailableNodes`, роль `customer-access-reader`, не принимает `customer_id`.
`app.ListAvailableNodes` вызывает один read-порт, а
`postgres.ListAvailableNodes` выполняет один SQL statement без транзакции из
нескольких шагов и без row locks.

Запрос соединяет актуальные `vpn_fleets`, `vpn_fleet_nodes` и `vpn_nodes`, берёт
`display_name` из `public_config` и сортирует результат по
`(vpn_fleet_id, node_id)`. PostgreSQL-адаптер одним проходом группирует строки в
fleets; пустые fleets из-за INNER JOIN не возникают. Customer-таблицы,
BRIDGE-связи и node-agent в этом пути не участвуют: доступность означает только
присутствие в текущей проекции manifest.

## Приём манифеста

`ApplyFleetManifest`, роль `manifest-writer`. Формат снапшота и правила валидации
описаны в [манифесте](MANIFEST.md), здесь только путь через код.

`app.ApplyFleetManifest.Execute`, `app/apply_fleet_manifest.go:57`:

1. `domain.ValidateManifest` до транзакции;
2. `LockManifestIngest` первым оператором: `pg_advisory_xact_lock` по одному
   фиксированному ключу, второй одновременный приём ждёт на нём;
3. `LoadProjection` читает принятое состояние целиком: `GetLastManifestRevision`,
   `ListCurrentNodeIDs`, `ListAcceptedFleetIDs`, `ListCurrentFleetMemberships`,
   `ListAllBridgeRoutes`;
4. `domain.PlanManifest` считает канонический digest (`domain/manifest_digest.go`)
   и сравнивает с принятым. Та же revision с тем же digest даёт
   `Idempotent: true`, и дальше не пишется ничего;
5. `WritePlan`;
6. `AppendAudit`, только если приём destructive.

`manifestTx.WritePlan`, `postgres/manifest.go:126`: `InsertManifestRevision`, затем
`writeNodes` (`UpsertVpnNode`, `RetireNodes`), `writeFleets` (`UpsertVpnFleet`,
`UpsertFleetNode`, `RetireFleetNodes`), `writeBridges` (`UpsertBridgeRoute`,
`RetireBridgeRoutes`), затем `InsertMaterializationJob` в той же транзакции.

Access customer здесь не меняются. Их раздаёт джоба.

## Материализация

Воркер `materialize`, `app/materialize_manifest.go:55`. Шаг обрабатывает одного
customer и укладывается в одну транзакцию.

`ClaimMaterializationJob` берёт lease самой старой незавершённой джобы; чужой
просроченный lease подбирается как свободный. `NextCustomerAfter` даёт первого
customer после курсора в порядке `customer_id`. Пустой результат означает конец
обхода: `CompleteMaterializationJob`. Обход идёт по всем customer, а не по
затронутым флотам.

`materializeCustomer`, `app/materialize_manifest.go:99`, читает в том же порядке,
что и команда: `LockEntitlement`, `LockOpenQuotaPeriod`, `LockNodeQuotaUsage`,
затем без locks `LoadTopology`, `LoadAccesses` и `LoadLiveNodes`.

`domain.PlanMaterialize`, `domain/materialize.go`, возвращает пять списков:

| список | что означает |
|---|---|
| `CreateAccesses` | цель появилась в манифесте |
| `DesiredChanges` | пересчёт desired state согласованных access |
| `Repoints` | у связи сменился `egress_tag`, идентичность access сохраняется |
| `Retire` | цель исчезла; `IssueOperation` разводит «доставить удаление» и «доставлять некуда» |
| `NodeQuotaInits` | у ноды нет строки расхода в текущем периоде |

`IssueOperation` зависит от `LoadLiveNodes`: если входной ноды нет в манифесте
глобально, удаление не доставляется, а `desired_version` всё равно растёт.

Согласованный customer даёт `IsNoOp`, и транзакция не трогает ни одной строки.
Иначе `WriteMaterialization`, `postgres/materialize.go:120`:
`BumpEntitlementDesiredVersion`, `InsertNodeQuotaUsage`, `writeTouchedNodes`,
`writeMaterializedAccesses` (`InsertVpnAccess`, `RepointAccess`, `RetireAccess`,
`UpdateAccessDesiredState`), `writeMaterializedOperations`. Курсор сдвигается
`AdvanceMaterializationCursor` в той же транзакции.

## Доставка операций

Воркер `dispatch`, `app/dispatch_operations.go:80`. Единственное место, откуда
backend ходит в сеть. Шаг это две транзакции с RPC между ними.

Первым оператором идёт `ReapExpiredOperationLeases`, не более 100 строк за шаг.
Операции с протухшим lease возвращаются в очередь с `next_attempt_at = now()`;
если `desired_version` access ушла вперёд, они становятся `SUPERSEDED`.

`LeaseNextOperation` (`queries/dispatch.sql`) одним запросом переводит операцию в
`IN_FLIGHT`, увеличивает `attempt_count` и собирает payload из актуальных строк
`vpn_accesses` и `vpn_nodes`. В `agent_operations` payload не хранится.
`encrypted_client_uuid` читается только для `ENSURE_PRESENT`. Запрос отбирает
`PENDING` и `RETRY_WAIT` с `next_attempt_at <= now()` при условии, что на ноде нет
`IN_FLIGHT`. Проигравший гонку получает unique violation по
`agent_operations_single_in_flight_per_node`; `isSingleInFlightViolation`,
`postgres/dispatch.go:156`, превращает его в «отправлять нечего».

`deliver`, `app/dispatch_operations.go:111`: если `AccessDesiredVersion` больше
`DesiredVersion` операции, вызова не будет, вернётся retryable-исход с кодом
`SUPERSEDED`. Иначе `Sealer.Open` для `PRESENT` и вызов
`nodeagent.Client.EnsureUserPresent` либо `EnsureUserAbsent`, deadline 5 секунд.
Ошибок эти методы не возвращают: транспорт и `ApplyStatus` из тела сводятся в один
`Outcome` внутри `internal/nodeagent`.

`record`, `app/dispatch_operations.go:166`, во второй транзакции:
`domain.PlanOperationResult` раскладывает исход, `SetAccessApplyState` обновляет
строку access с условием на `desired_version`. Ноль затронутых строк означает, что
версия уехала: план проходит через `Superseded()`. Затем `CompleteOperation`
пишет статус, `next_attempt_at`, `last_error_code` и снимает lease.

## Жизненный цикл операции

```
PENDING → IN_FLIGHT → SUCCEEDED
   |          ├→ RETRY_WAIT → IN_FLIGHT
   |          ├→ FAILED_PERMANENT
   |          └→ SUPERSEDED
   └→ SUPERSEDED
```

Задержка повтора: экспонента от 1 секунды до 5 минут, `domain.BackoffDelay`. Нижняя
половина фиксирована, jitter добавляет до половины сверху. Предела числа попыток
нет.

`attempt_count` увеличивается при взятии lease, а не при записи результата, поэтому
первая неудача приходит в домен со счётчиком 1.

В `SUPERSEDED` операция попадает четырьмя способами:

| способ | где |
|---|---|
| смена desired state гасит висящие операции access в своей же транзакции | `SupersedeStaleOperations`: `apply.go:320`, `materialize.go:269`, `usage.go:416`, `expiry.go:127` |
| гейт перед RPC вернул retryable с кодом `SUPERSEDED` | `app/dispatch_operations.go:127` |
| `SetAccessApplyState` не нашёл строку с этой `desired_version` | `app/dispatch_operations.go:180` |
| сборщик нашёл `IN_FLIGHT` с устаревшей версией | `queries/dispatch.sql` |

Первый способ основной: при обычном изменении desired state операции гасятся сразу,
до диспетчера. Три остальных закрывают гонки.

Есть и пятый выход из очереди, минующий диспетчера:
`CompleteNodeOperationsByReconcile` переводит все `PENDING` и `RETRY_WAIT` ноды в
`SUCCEEDED` с кодом `RECONCILE_APPLIED`, потому что полный набор уже доставил
desired state.

## Опрос трафика

Воркер `usage`, `app/pull_usage.go:85`. Агент в базу не пишет: backend тянет у него
дельты и начисляет сам.

`ClaimUsageNode` берёт lease ноды, которую пора опросить, и отдаёт подтверждённую
позицию курсора. Выборка идёт по `vpn_nodes` с `current`, поэтому снятая с
манифеста нода опрашиваться перестаёт, хотя строка курсора у неё остаётся. `GetNodeState`, `nodeagent/usage.go:88`, забирает не более 16
порций за вызов, deadline 30 секунд. Ответ несёт ещё и `needs_bootstrap`, который
уезжает в `SetNodeNeedsBootstrap`: это единственный источник признака, и читает его
`reconcile`.

Каждая порция обрабатывается отдельно, `consumeBatch`, `app/pull_usage.go:153`:

1. `ResolveAccountingIDs` сопоставляет `accounting_id` с владельцами по
   историческому маппингу, включая ретайрнутые access;
2. неопознанные и пришедшие с чужой ноды уходят в `QuarantineUsageItems`
   отдельной транзакцией;
3. `domain.GroupUsageItems` режет остаток на группы. Ключ группы это
   `(customer_id, node_id)`, период в него не входит: его выбирает уже сама
   транзакция группы по времени сбора порции.

Группа это одна транзакция, `consumeGroup`, `app/pull_usage.go:221`: `Now`,
`LockEntitlement`, `LockQuotaPeriodAt` по времени сбора порции,
`EnsureNodeQuotaUsageRow` и `LockNodeQuotaUsageRow`, затем
`RegisterProcessedUsageItems`. Последний возвращает только новые items: уже
зарегистрированные не начисляются повторно. Пустой результат завершает группу.

`ListCustomerNodeAccesses` читается только когда порог в принципе может сработать.
`domain.PlanUsageGroup` даёт начисление, `exhausted_at` и список гасимых access.
`WriteUsageGroup`, `postgres/usage.go:362`: `AddNodeQuotaUsage`,
`SetNodeQuotaExhausted`, `LockNodesForUpdate` с `BumpNodesDesiredRevision`,
`UpdateAccessDesiredState`, supersede и `InsertAgentOperation`.

`AdvanceUsageCursor` вызывается после durable commit всех групп порции. Смена
`spool_id` сбрасывает сравнение последовательности и пишется в лог уровня ERROR:
часть трафика на ноде потеряна. Lease снимается `ReleaseUsageLease` на любом
выходе.

## Сверка ноды

Воркер `reconcile`, `app/reconcile_nodes.go:98`. Единственный источник удалений на
ноде: self-heal на агенте только добавляет.

`ClaimNodeForReconcile` одной транзакцией берёт lease, фиксирует
`desired_revision` и читает полный набор (`ListNodeDesiredUsers`), плюс `flow` из
`public_config` и признак `needs_bootstrap`.

`materialize`, `app/reconcile_nodes.go:187`, расшифровывает набор целиком. Пустой
`flow` или любая неудача `Open` прекращают шаг: набор авторитетен, и пропуск одного
юзера означал бы его удаление с ноды.

Если `needs_bootstrap` не выставлен, перед дорогим вызовом идёт проверка на дрейф,
`drifted`, `app/reconcile_nodes.go:148`. `ObserveUsers` забирает фактический
инвентарь Xray, `app.CompareInventory` сравнивает его с набором по
`accounting_id`, сверяя `client_uuid`, `flow` и `egress_key`. Виды расхождений:
`missing`, `extra`, `mismatch`. Непригодное наблюдение (`incomplete`,
`not_observed`, `stale`, `clock_skewed`) отменяет сверку целиком, и набор не
уезжает.

`ReconcileUsers`, `nodeagent/reconcile.go:46`, отправляет набор, deadline 60 секунд:
до 2000 юзеров, каждого агент применяет к Xray по одному. `AcceptReconcile`,
`postgres/reconcile.go:106`: `AcceptNodeReconcile` с условием на `desired_revision`
(ноль строк означает, что desired state уехал за время вызова, и транзакция
откатывается пустой), `MarkNodeAccessesApplied`,
`CompleteNodeOperationsByReconcile`.

## Истечение срока

Воркер `expiry`, `app/expire_customers.go:36`. Lease и джобы у него нет.

`LockNextDueExpiredCustomer` берёт `FOR UPDATE SKIP LOCKED` одного customer с
наступившим `expires_at`, у которого остался хотя бы один `PRESENT`-access.
`expires_at` перечитывается под locком: `domain.PlanExpiry` даёт `IsNoOp` для
продлённого customer, и шаг просто засчитывается прогрессом.

Квоту путь не читает вовсе: у истёкшего customer desired state равен `ABSENT` при
любом её состоянии. `WriteExpiry`, `postgres/expiry.go:72`:
`BumpEntitlementDesiredVersion`, `writeExpiredNodes`, `UpdateAccessDesiredState` по
каждому гасимому access, `writeExpiryOperations`. Затем `AppendAudit`.

## Уборка и снимок метрик

`prune-usage-dedup`, `app/prune_usage_dedup.go:49`, вызывает
`PruneProcessedUsageItems` с окном 6 часов и потолком 5000 строк за шаг. Шаг,
удаливший ровно потолок, идёт на следующий без паузы.

`stats`, `app/stats.go`, читает `StatsAgentOperations`, `StatsAccesses`,
`StatsNodeCursors`, `StatsQuarantine`, `StatsScalars` и версию схемы, после чего
`internal/metrics/stats.go` раскладывает результат по gauge. Снимок делает воркер,
а не обработчик `/metrics`.

Вторая половина метрик снимается декораторами портов, `internal/metrics/agent.go`
и `sealer.go`: latency и коды исходов вызовов агента, состояние ноды по последнему
опросу, дрейф, отказы расшифрования. Метки `customer_id` нет ни у одной метрики,
это проверяется тестом по белому списку.

Колонки, типы, индексы и ретенция описаны в [базе данных](DATABASE.md).
