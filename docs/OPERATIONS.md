# Эксплуатация

Что процесс рассказывает о себе и как этим пользоваться, когда что-то не работает.

Три источника: служебный HTTP-порт с проверками готовности, метрики Prometheus и
structured logs в stderr. Первый отвечает «жив ли процесс», второй «идёт ли
работа», третий «что именно случилось с конкретной нодой или командой».

## Куда смотреть первым делом

| симптом | первая проверка | раздел |
|---|---|---|
| команды product-сервиса не проходят | `/health/ready`, затем `grpc_code` в логе | Служебный порт, Чтение логов |
| ссылки выданы, но не работают | `spiritvpn_accesses{apply_state="FAILED"}` и `{apply_state="RETRYING"}` | Залипшие операции |
| трафик не начисляется | `spiritvpn_node_usage_pull_age_seconds` по нодам | Недоступная нода |
| ссылка перестала работать у части клиентов | `spiritvpn_exhausted_node_quotas`, `spiritvpn_expired_customers` | Квота и срок |
| метрики выглядят правдоподобно, но не меняются | `spiritvpn_stats_last_refresh_timestamp_seconds` | Метрики, самонаблюдение |
| новая ревизия манифеста не доехала до нод | `spiritvpn_manifest_revision` против `spiritvpn_manifest_materialized_revision` | Метрики, манифест |

## Служебный порт

`SPIRIT_HTTP_LISTEN`, по умолчанию `:8080`. Сервер собирается в
`cmd/spiritvpnd/health.go:30`, на нём три обработчика: `GET /health/live`,
`GET /health/ready`, `GET /metrics`.

TLS у порта нет, клиентский сертификат он не спрашивает, идентичность вызывающего
не проверяет ни на одном из трёх путей. Любой, кто дотянулся до порта по сети,
читает `/metrics` целиком, а там перечислены все `node_id` флота, объёмы
подтверждённого трафика и глубина очередей. Ограничение одно, и это сетевая
доступность порта.

`ReadHeaderTimeout` 5 секунд: соединение, не приславшее заголовки, отпускается
само.

### /health/live

Отвечает `200 ok` всегда, `handleLive`, `cmd/spiritvpnd/health.go:50`. Ни базы,
ни нод, ни ключа шифрования он не касается.

Отказ liveness означает ровно одно: процесс не принимает соединения на служебном
порту. О состоянии PostgreSQL он не говорит ничего, и искать по нему причину
деградации бесполезно.

### /health/ready

Три проверки подряд, `readinessChecks`, `cmd/spiritvpnd/health.go:84`. Порядок
фиксирован по возрастанию стоимости, первая же неудача прекращает обход. На все
три вместе отведено 3 секунды (`readinessTimeout`), после чего проверка
считается неуспешной.

Успех это `200 ready`. Неудача это `503` с телом `not ready: <имя проверки>`.
Больше в ответе ничего нет: текст ошибки pgx содержит параметры подключения, и
наружу он не уходит.

| имя в ответе | что проверяет | что делать |
|---|---|---|
| `postgres` | `pool.Ping` | база недоступна или исчерпан пул, смотреть `spiritvpn_db_pool_*` в предыдущих scrape |
| `schema` | `SELECT version, dirty FROM schema_migrations` | либо миграции не накатаны до нужной версии, либо прерванная миграция оставила `dirty` |
| `encryption_key` | `cipher.SelfTest()` | `SPIRIT_CLIENT_UUID_KEY` не тот, что использовался при записи |

Проверка `schema` требует версию не ниже встроенной в бинарь, `schemaCheck`,
`cmd/spiritvpnd/health.go:107`. Схема новее бинаря готовность не отменяет. Пара
`spiritvpn_binary_schema_version` ниже `spiritvpn_schema_version` это нормальное
состояние во время выкатки, а не повод искать поломку; тревожит обратное
соотношение и `spiritvpn_schema_dirty` равный единице.

## Метрики

Префикс всех метрик `spiritvpn`, `internal/metrics/registry.go:28`. Строки HELP
английские. Метки `customer_id` нет ни у одной метрики, и это проверяется тестом
по белому списку.

Всего 37 серий: 33 объявлены в `internal/metrics/registry.go`, ещё 4 отдаёт
коллектор пула соединений, `internal/metrics/pool.go:24`. Сверх них в выдаче есть
стандартные `go_*` и `process_*`.

Метрики делятся по происхождению, и от этого зависит, насколько свежее значение
вы читаете.

Первая половина снимается декораторами портов в момент вызова: `WrapAgent`
(`internal/metrics/agent.go:29`), `WrapSealer` (`sealer.go:18`),
`WrapUsageRetention` (`retention.go:20`). Эти значения актуальны на момент
scrape.

Вторая половина приезжает снимком состояния БД, который делает воркер `stats`
раз в 15 секунд (`statsRefreshInterval`, `cmd/spiritvpnd/worker.go:161`).
Обработчик `/metrics` в базу не ходит и отдаёт последний снимок из памяти, так
что эти значения отстают на интервал снимка.

Вектор, в который ни разу ничего не записали, в `/metrics` отсутствует целиком.
Предзаполнены нулями только `spiritvpn_agent_operations`, `spiritvpn_accesses`
(`seedEnums`, `registry.go:374`) и оба исхода
`spiritvpn_stats_refreshes_total`. До первого похода к ноде серий
`spiritvpn_agent_*` в выдаче нет, и отсутствие серии в такой момент не означает
нуля.

### Самонаблюдение

Читать первыми. Отказавший воркер снимка выглядит как застывшие, но
правдоподобные числа во всех метриках второй половины.

| метрика | метки | что означает отклонение |
|---|---|---|
| `spiritvpn_stats_last_refresh_timestamp_seconds` | нет | возраст заметно больше 15 секунд означает, что весь снимок БД устарел и остальные gauge читать нельзя |
| `spiritvpn_stats_refreshes_total` | `result`: `ok`, `error` | растущий `error` при неподвижном `ok` означает, что база не отдаёт снимок; текст в логе воркера `stats` |
| `spiritvpn_schema_version` | нет | версия схемы в базе |
| `spiritvpn_binary_schema_version` | нет | версия миграций в бинаре; выше `schema_version` означает, что процесс не готов |
| `spiritvpn_schema_dirty` | нет | единица означает прерванную миграцию, процесс не готов |

### Очередь операций

| метрика | метки | что означает отклонение |
|---|---|---|
| `spiritvpn_agent_operations` | `status`: шесть статусов | накопление в `PENDING` или `RETRY_WAIT` означает, что очередь не разбирается |
| `spiritvpn_agent_operation_oldest_age_seconds` | `status` | возраст самой старой операции статуса по `created_at` |
| `spiritvpn_accesses` | `desired_state`, `apply_state` | ненулевой `FAILED` означает access, до которого диспетчер больше не дойдёт |

Возраст осмыслен только для `PENDING` и `RETRY_WAIT`. Строки терминальных
статусов из таблицы не удаляются, и их возраст растёт вечно при полностью
исправной системе.

`spiritvpn_accesses` считает только неретайрнутые строки. Восемь серий, пара
`desired_state` (`PRESENT`, `ABSENT`) на `apply_state` (`PENDING`, `APPLIED`,
`RETRYING`, `FAILED`).

### Ноды

| метрика | метки | что означает отклонение |
|---|---|---|
| `spiritvpn_agent_calls_total` | `node`, `method`, `code` | распределение исходов вызовов; коды перечислены в `internal/nodeagent/outcome.go:19` |
| `spiritvpn_agent_call_duration_seconds` | `method`, `code` | гистограмма до 30 секунд; метки `node` у неё нет |
| `spiritvpn_agent_last_success_timestamp_seconds` | `node` | возраст больше минуты означает, что нода не отвечает: `GetNodeState` идёт к каждой ноде раз в 15 секунд |
| `spiritvpn_agent_alert_outcomes_total` | `node`, `code` | любой прирост требует разбора; сюда попадают `IDENTITY_MISMATCH`, `NODE_CONFIG_INVALID`, `AGENT_UNKNOWN_STATUS`, `UNAUTHENTICATED`, `PERMISSION_DENIED`, `UNIMPLEMENTED`, `TRANSPORT` |
| `spiritvpn_node_xray_reachable` | `node` | ноль означает, что агент жив, а Xray на ноде нет |
| `spiritvpn_node_xray_uptime_seconds` | `node` | сброс к малому значению означает перезапуск Xray |
| `spiritvpn_node_needs_bootstrap` | `node` | единица означает, что агенту запрещено удалять юзеров до полного набора |
| `spiritvpn_reconcile_drift_total` | `node`, `kind`: `added`, `replaced`, `removed` | устойчиво ненулевой rate означает, что состояние ноды расходится молча |
| `spiritvpn_inventory_observations_total` | `node`, `result`: `complete`, `incomplete`, `not_observed` | нода, у которой не бывает `complete`, из сверки выпадает и расхождений не показывает |

Значения `method`: `EnsureUserPresent`, `EnsureUserAbsent`, `GetNodeState`,
`ReconcileUsers`, `ObserveUsers`. Последний это тот же RPC `GetNodeState` с
`include_users`, разведённый в отдельную метку по цене вызова.

Три gauge `node_xray_reachable`, `node_xray_uptime_seconds` и
`node_needs_bootstrap` показывают последнее наблюдённое состояние, а не текущее.
Снятая с манифеста нода перестаёт опрашиваться, и её серии застывают на
последнем значении навсегда. Читать их без
`spiritvpn_agent_last_success_timestamp_seconds` нельзя: `reachable=1` у ноды,
последний успех которой был вчера, означает вчерашний день. Авторитетный состав
флота даёт `spiritvpn_node_usage_pull_age_seconds`, он приезжает снимком БД и
снятую ноду теряет.

### Трафик

| метрика | метки | что означает отклонение |
|---|---|---|
| `spiritvpn_node_usage_pull_age_seconds` | `node` | время с последней попытки опроса; больше 15 секунд с запасом означает, что воркер до ноды не доходит |
| `spiritvpn_node_usage_acked_sequence` | `node` | остановка при растущем `pull_age` и есть backlog на ноде |
| `spiritvpn_node_usage_lease_expired` | `node` | единица означает, что держатель lease умер, не сняв его |
| `spiritvpn_usage_pull_capped_total` | `node` | агент отдал ровно потолок порций, в спуле осталось ещё; устойчивый прирост означает, что опрос не успевает |
| `spiritvpn_traffic_quarantine_rows` | `reason`: `UNKNOWN_ACCOUNTING_ID`, `NO_QUOTA_PERIOD` | число строк карантина, растёт монотонно до ретенции, alert строится на `rate()` |
| `spiritvpn_usage_dedup_oldest_age_seconds` | нет | в норме колеблется около окна ретенции в 6 часов и к нулю не идёт |
| `spiritvpn_usage_dedup_rows_pruned_total` | нет | неподвижен при растущем возрасте самой старой записи означает отказ уборки |

Возраст дедуп-записей превышает окно по двум причинам, и метрика их не
различает: либо встал воркер `prune-usage-dedup`, либо у какой-то ноды давно не
двигался курсор и её строки нельзя удалять как неподтверждённые. Разделяются они
по `spiritvpn_node_usage_pull_age_seconds` из той же выдачи.

### Манифест

| метрика | метки | что означает отклонение |
|---|---|---|
| `spiritvpn_manifest_revision` | нет | последняя принятая ревизия |
| `spiritvpn_manifest_materialized_revision` | нет | последняя ревизия, чей fan-out по всем customer завершён |
| `spiritvpn_manifest_materialization_lag_seconds` | нет | возраст самой старой незавершённой джобы; ноль означает, что незавершённых нет |

Отставание `materialized_revision` от `manifest_revision` означает, что новая
топология до части customer ещё не доехала. Ноль в `materialization_lag_seconds`
при таком отставании читается как «джоба не создана или уже помечена `FAILED`»,
а не как «лага нет».

### Сроки и квоты

| метрика | метки | что означает отклонение |
|---|---|---|
| `spiritvpn_expired_customers` | нет | customer с наступившим `expires_at`, у которых остался хотя бы один `PRESENT`-access |
| `spiritvpn_expiry_lag_seconds` | нет | насколько просрочен самый старый непогашенный customer |
| `spiritvpn_exhausted_node_quotas` | нет | ноды с выставленным `exhausted_at` в открытом периоде квоты |

Первые две строятся на том же предикате, что и `LockNextDueExpiredCustomer`, и
меряют ровно ту очередь, которую разбирает воркер `expiry`. Что снимает каждое из
трёх состояний, описано ниже, в разделе «Квота и срок».

### Процесс

| метрика | метки | что означает отклонение |
|---|---|---|
| `spiritvpn_worker_leases_held` | `worker`: `dispatch`, `materialize`, `usage` | занятость, штатная работа |
| `spiritvpn_worker_leases_expired` | `worker` | держатель умер, не сняв lease; работу подберёт следующий шаг |
| `spiritvpn_credential_open_errors_total` | нет | любое ненулевое значение означает расхождение ключа шифрования |
| `spiritvpn_db_pool_connections` | `state`: `total`, `idle`, `acquired`, `constructing` | `acquired`, упёршийся в `spiritvpn_db_pool_max_connections`, означает исчерпание пула |
| `spiritvpn_db_pool_max_connections` | нет | `SPIRIT_DB_MAX_CONNS`, по умолчанию 10 |
| `spiritvpn_db_pool_acquires_total` | `result`: `total`, `empty`, `canceled` | растущий `empty` означает, что захваты ждут свободного соединения |
| `spiritvpn_db_pool_acquire_wait_seconds_total` | нет | суммарное ожидание соединения |

Значения `expiry` среди воркеров нет: он координируется
`FOR UPDATE SKIP LOCKED` на строке customer, и lease у него отсутствует.
Показывать по нему нечего, а искать зависший `expiry` следует по
`spiritvpn_expiry_lag_seconds`.

`credential_open_errors_total` не различает места отказа. Расшифровка идёт двумя
путями, диспетчер собирает payload перед RPC и выдача ссылок строит VLESS URI, и
оба гасят по одной единице работы молча: ссылка не отдаётся, операция уходит в
повтор.

### Чего в метриках нет

Искать серию под эти три наблюдения бесполезно.

| наблюдение | где взять |
|---|---|
| потеря спула на ноде, то есть безвозвратно потерянный трафик | запись ERROR «спул ноды сменился, часть трафика не учтена», `internal/app/pull_usage.go:384` |
| расхождения, найденные сверкой до отправки набора | запись WARN «нода разошлась с desired state» с полями `missing`, `extra`, `mismatch`, `internal/app/reconcile_nodes.go:173` |
| глубина спула на агенте | нигде, backend её не знает; ближайшее приближение это `spiritvpn_usage_pull_capped_total` |

Второй пункт легко спутать с `spiritvpn_reconcile_drift_total`: тот считает
изменения, которые применил `ReconcileUsers` (`added`, `replaced`, `removed`),
и появляется только после успешной отправки набора. Расхождения, найденные
`app.CompareInventory` до отправки, метрикой не покрыты.

## Залипшие операции

Строка `agent_operations` живёт по циклу, описанному в
[архитектуре](ARCHITECTURE.md). Здесь только то, как каждый статус выглядит
снаружи и что с ним делать.

| статус | как выглядит | разберётся ли сам |
|---|---|---|
| `PENDING` | создана, ещё не взята диспетчером | да, пока жив хоть один из восьми циклов доставки |
| `IN_FLIGHT` | взята под lease на 30 секунд, RPC в полёте | да; возраст больше lease означает умершего держателя, и это же видно в `worker_leases_expired{worker="dispatch"}` |
| `RETRY_WAIT` | ждёт `next_attempt_at`, пауза до 5 минут | да, число попыток не ограничено |
| `FAILED_PERMANENT` | терминальная, повторов не будет | нет |

`PENDING` с растущим возрастом при нулевом `IN_FLIGHT` означает, что диспетчер до
очереди не доходит вовсе. Все восемь горутин стоят на отказе базы: искать запись
«шаг воркера отказал» с `worker=dispatch`, там же и текст ошибки.

`RETRY_WAIT` в накоплении это чаще всего одна недоступная нода. Разбирать по
`last_error_code`:

```sql
SELECT node_id, last_error_code, count(*), min(created_at)
FROM agent_operations
WHERE status = 'RETRY_WAIT'
GROUP BY node_id, last_error_code
ORDER BY count DESC;
```

`FAILED_PERMANENT` повторно не берётся никогда. Ровно восемь кодов приводят
операцию сюда, `classify` и `classifyTransport`, `internal/nodeagent/outcome.go:60`
и `:95`: `AGENT_PERMANENT`, `AGENT_UNKNOWN_STATUS`, `INVALID_ARGUMENT`,
`FAILED_PRECONDITION`, `UNAUTHENTICATED`, `PERMISSION_DENIED`, `UNIMPLEMENTED`,
`IDENTITY_MISMATCH`. Четыре последних чинятся вне backend: это сертификаты и
версия агента.

В этом списке нет `CREDENTIAL_UNREADABLE`, отказа расшифровать `client_uuid`
(`internal/app/dispatch_operations.go:150`). Он классифицирован retryable с
alert, и операция с ним уходит в бесконечный `RETRY_WAIT`. Ищите такую очередь по
ненулевому `spiritvpn_credential_open_errors_total`: сменой desired state она не
лечится, расшифровка провалится и в следующем поколении.

Восстанавливается после `FAILED_PERMANENT` не операция, а access. Два пути:

1. новая команда product-сервиса двигает `desired_version`, и появляется новая
   операция;
2. воркер `reconcile` в течение 5 минут отправляет ноде полный набор, после чего
   `MarkNodeAccessesApplied` переводит `apply_state` в `APPLIED`.

Второй путь срабатывает сам, но только если сверка проходит целиком: набор
авторитетен, и любая неудача по дороге (нода не ответила, `client_uuid` не
расшифровался, `flow` пустой) отменяет шаг и оставляет `apply_state` в `FAILED`
до следующего интервала.

Саму строку `FAILED_PERMANENT` не трогает ни один из путей.
`CompleteNodeOperationsByReconcile` закрывает только `PENDING` и `RETRY_WAIT`
(`internal/postgres/queries/reconcile.sql:138`), терминальная остаётся записью
журнала исполнения. Счётчик `agent_operations{status="FAILED_PERMANENT"}` от
этого растёт монотонно, и alert на его абсолютное значение сработает один раз и
навсегда. Действующее состояние показывает
`spiritvpn_accesses{apply_state="FAILED"}`.

## Недоступная нода

Порядок появления признаков, если нода перестала отвечать:

1. в пределах 15 секунд перестаёт расти
   `spiritvpn_agent_last_success_timestamp_seconds{node}`: `GetNodeState` ходит
   к каждой ноде каждый интервал опроса, и это самый ранний признак;
2. `spiritvpn_agent_calls_total{node,code="UNAVAILABLE"}` начинает расти, для
   зависшего агента вместо него `DEADLINE_EXCEEDED`;
3. `spiritvpn_node_usage_pull_age_seconds{node}` растёт, `acked_sequence` стоит:
   трафик копится в спуле агента;
4. `spiritvpn_agent_operations{status="RETRY_WAIT"}` растёт, если на эту ноду в
   этот момент были адресованы операции.

### Что система делает сама

Доставка уходит в экспоненциальный backoff с потолком 5 минут и не сдаётся:
`UNAVAILABLE` и `DEADLINE_EXCEEDED` классифицированы как retryable, и предела
попыток нет. Опрос трафика продолжает подходить к ноде раз в 15 секунд и после
восстановления заберёт весь накопленный спул, начиная с подтверждённой позиции.
Сверка приходит раз в 5 минут и приводит ноду к desired state одним полным
набором.

Короткая недоступность вмешательства не требует вообще: после возврата ноды все
четыре признака рассасываются без участия оператора, а трафик за время простоя
начисляется задним числом.

### Когда вмешиваться

| признак | что это |
|---|---|
| растёт `spiritvpn_agent_alert_outcomes_total{code="IDENTITY_MISMATCH"}` | нода предъявила сертификат с чужой идентичностью |
| растёт `{code="NODE_CONFIG_INVALID"}` | в манифесте у ноды непригодный `agent_config`, чинится следующей ревизией манифеста |
| растёт `{code="TRANSPORT"}` | неопознанная ошибка транспорта, разбирать по полю `message` в записи «опрос ноды не удался» |
| `spiritvpn_node_xray_reachable{node}` равен нулю при свежем `last_success` | агент жив, Xray на ноде нет; операции на неё будут повторяться без конца |

Что происходит с манифестом, пока нода молчит, описано в
[MANIFEST.md](MANIFEST.md).

## Квота и срок

Оба состояния гасят access одинаково: `desired_state` становится `ABSENT`,
операция уезжает на ноду, юзер удаляется из Xray. Клиент видит переставшую
работать ссылку. Различаются они тем, что снимает блокировку.

### Исчерпанная квота

`spiritvpn_exhausted_node_quotas` считает ноды с выставленным `exhausted_at`
внутри открытого периода. Единица счёта нода, не customer: `exhausted_at` живёт
в `node_quota_usage`, и гасятся все access этого customer на этой ноде. Закрытые
периоды в метрику не входят.

Тот же учтённый расход доступен product-сервису в `consumed_bytes` ответа
`GetCustomerAccessLinks`, рядом с `usage_quota_bytes`. У FREEDOM поля относятся к
самой ноде, у BRIDGE — к entry-ноде; ссылки с общей entry-нодой показывают одно
значение. Для остатка используется
`max(usage_quota_bytes - consumed_bytes, 0)`, а не беззнаковое вычитание.

Это не live-счётчик Xray. Usage-воркер опрашивает node-agent не чаще одного раза
в 15 секунд на ноду, а при недоступности ноды расход остаётся у агента и попадёт
в backend после восстановления. Поэтому `consumed_bytes` может временно
отставать, затем вырасти скачком и превысить лимит; это штатно.

Снимают исчерпание две разные команды, и различает их только `expires_at`,
`ClassifyApply`, `internal/domain/command.go:91`:

| команда | что происходит с расходом |
|---|---|
| `expires_at` строго больше сохранённого (`RENEWAL`) | текущий период квоты закрывается, открывается новый с нулевым расходом, исчерпанных нод не остаётся ни одной |
| `expires_at` тот же, `usage_quota_bytes` больше (`QUOTA_CHANGE`) | период тот же, накопленный трафик остаётся, `RecomputeExhausted` снимает `exhausted_at` только с тех нод, чей расход стал ниже нового лимита |

Из этого следует, что поднятие квоты без продления возвращает не всех: нода, уже
перебравшая и новый лимит, останется исчерпанной, и её access останутся
погашенными.

### Истёкший срок

`spiritvpn_expired_customers` считает customer с наступившим `expires_at`, у
которых остался хотя бы один `PRESENT`-access. `spiritvpn_expiry_lag_seconds`
показывает, насколько просрочен самый старый из них.

При пустой очереди обе метрики равны нулю: `expiry_lag_seconds` считается по
тому же множеству и при его отсутствии сворачивается в ноль
(`internal/postgres/queries/stats.sql:108`). Ноль в лаге сам по себе ничего не
доказывает, читать его нужно вместе со счётчиком. Устойчиво растущий лаг при
ненулевом счётчике означает, что воркер `expiry` не работает, а не что клиентов
много.

Снимается продлением: `ApplyCustomerAccess` с `expires_at` дальше сохранённого.
Сокращение срока отвергается кодом `EXPIRY_REGRESSION`.

Все три метрики раздела приезжают снимком БД, то есть с задержкой до 15 секунд.

## Чтение логов

JSON в stderr, `newLogger`, `cmd/spiritvpnd/logging.go:17`. Уровень задаёт
`SPIRIT_LOG_LEVEL`, по умолчанию `info`.

Одна запись на каждый gRPC-вызов, `LoggingUnaryInterceptor`,
`internal/grpcsvc/logging.go:22`. Поля записи и стабильные коды ошибок описаны в
[API.md](API.md).

Корреляция идёт по `request_id`. Он кладётся в контекст первым interceptor'ом и
уезжает оттуда в две стороны: в запись `grpc` и в записи pgx, у метода `Log`
трейсера тот же контекст под рукой (`pgxLogger`, `cmd/spiritvpnd/logging.go:33`).
Запрос к базе и породивший его RPC сходятся по одному значению. На уровне `info`
записей pgx нет: трейсер переключается на подробный режим только при
`SPIRIT_LOG_LEVEL=debug`, `tracerLevel`, `cmd/spiritvpnd/logging.go:70`.

У записей фоновых воркеров `request_id` пустой: входящего вызова у них нет, а
`pgxLogger` кладёт поле безусловно. Корреляция там идёт по `node_id`.

Чего в логах нет никогда: тела запроса и ответа RPC, аргументов SQL-запросов.
Аргументы отбрасываются в `pgxLogger` до записи, а не уровнем логирования, так
что включение `debug` на проде их не откроет.

Записи, за которыми ходят чаще всего:

| сообщение | уровень | где | о чём |
|---|---|---|---|
| `шаг воркера отказал` | ERROR | `cmd/spiritvpnd/worker.go:212` | поле `worker` называет отказавший цикл, дальше пауза 15 секунд |
| `спул ноды сменился, часть трафика не учтена` | ERROR | `internal/app/pull_usage.go:384` | нода потеряла спул, трафик за окно потерян безвозвратно |
| `опрос ноды не удался` | WARN или ERROR | `internal/app/pull_usage.go:366` | ERROR при `Alert` у исхода |
| `нода разошлась с desired state` | WARN | `internal/app/reconcile_nodes.go:173` | поля `missing`, `extra`, `mismatch` с числом расхождений |
| `инвентарь ноды непригоден для сверки` | INFO | `internal/app/reconcile_nodes.go:163` | поле `reason`; штатно для только что поднявшегося агента, тревожит повторяемость |
| `reconcile пропущен: client_uuid не расшифровывается` | ERROR | `internal/app/reconcile_nodes.go:204` | сверка ноды остановлена целиком, парная метрика `credential_open_errors_total` |
| `reconcile пропущен: public_config ноды непригоден` | ERROR | `internal/app/reconcile_nodes.go:193` | в манифесте у ноды пустой `flow` |
| `поверхность отказала` | ERROR | `cmd/spiritvpnd/main.go:266` | отказал слушатель gRPC или HTTP, следом идёт shutdown |

Уровень записи `grpc` выбирает `levelFor`, `internal/grpcsvc/logging.go:51`.
ERROR там означает вину backend (`INTERNAL`, `UNKNOWN`, `DATA_LOSS`,
`UNAVAILABLE`), WARN означает отказ по правилам контракта, то есть штатную работу
сервиса. Alert по потоку ошибок строить нужно на ERROR: продление с сокращением
срока или неизвестный флот приходят от вызывающего и в разборе не нуждаются.

## Старт и остановка

Отказ старта не бывает записью JSON. Любая ошибка, возвращённая из `run`,
печатается `main` одной обычной строкой в stderr с префиксом `spiritvpnd:`
(`cmd/spiritvpnd/main.go:52`), и выход идёт с кодом 1. Логгер к этому моменту
чаще всего уже собран, но в этот путь он не подключён, так что искать причину
grep'ом по полю `msg` бесполезно.

Порядок инициализации в `run`, `cmd/spiritvpnd/main.go:60`. Валит старт всё
перечисленное:

| шаг | что валит | текст |
|---|---|---|
| `config.Load` | переменная не задана, значение не разбирается, ключ не в формате `<key_id>:<base64>` или не той длины | `config: обязательная переменная не задана`, `config: некорректное значение`, ошибки `crypto:` по ключу |
| `crypto.NewCipher` и `SelfTest` | ключ разобрался, но шифр на нём не работает | `шифр client_uuid`, `самопроверка ключа шифрования` |
| `newPool` | DSN не разбирается, база не ответила за 10 секунд | `SPIRIT_DATABASE_URL не разбирается как DSN PostgreSQL`, `подключение к PostgreSQL` |
| `nodeagent.New` | не читается клиентская пара или CA агентов, CA без единого сертификата | `nodeagent: клиентская пара TLS`, `nodeagent: CA агентов` |
| `newGRPCServer` | не читается серверная пара или CA клиентов | `серверная пара TLS`, `CA клиентов` |
| `net.Listen` в `serve` | порт занят | `прослушивание gRPC на ...`, `прослушивание HTTP на ...` |

Длину и формат ключа проверяет разбор конфигурации, а не `NewCipher`, так что
испорченный `SPIRIT_CLIENT_UUID_KEY` падает первым шагом.

DSN в текст ошибки не попадает: в нём пароль. При неразбираемом
`SPIRIT_DATABASE_URL` наружу уходит только имя переменной.

Слушатели открываются до запуска горутин, так что занятый порт становится
ошибкой старта. Фоновые воркеры при этом уже запущены и работают: от сетевых
поверхностей они не зависят.

Перехват `SIGTERM` и `SIGINT` стоит до инициализации, и сигнал прерывает её тоже.
Процесс, застрявший на недоступной базе, останавливается сразу, не дожидаясь
конца grace period.

Остановка идёт в три шага, `shutdown`, `cmd/spiritvpnd/main.go:278`:

1. `grpcServer.GracefulStop` перестаёт принимать соединения и ждёт завершения
   текущих RPC, бюджет 20 секунд (`shutdownTimeout`);
2. по исчерпании бюджета пишется WARN «graceful shutdown не уложился в бюджет,
   рву соединения» и вызывается `Stop`;
3. `httpServer.Shutdown` на остатке того же бюджета.

Бюджет у шагов общий, так что срабатывание второго шага съедает третий: следом за
записью «graceful shutdown не уложился в бюджет» всегда идёт «HTTP не завершился
корректно». Вторая запись при этом ничего нового не сообщает.

Воркеры останавливаются последними, уже после закрытия обеих поверхностей.
Ожидание короткое: шаг каждого мал, а весь прогресс зафиксирован в БД.

Операция, чей RPC оборвала отмена, остаётся `IN_FLIGHT` с lease на 30 секунд.
Её подберёт `ReapExpiredOperationLeases`: сбор идёт в начале каждого шага
диспетчера, до 100 строк за раз (`internal/app/dispatch_operations.go:81`).
Операция с актуальной `desired_version` вернётся в `RETRY_WAIT` с
`next_attempt_at = now()`, устаревшая станет `SUPERSEDED`. Дополнительной паузы
после рестарта нет, TTL lease ею уже был.

Счётчик `attempt_count` она увеличить успела, так что её следующая неудача
придёт в домен с увеличенным счётчиком и получит более долгую паузу. Всплеск
`spiritvpn_worker_leases_expired{worker="dispatch"}` сразу после рестарта штатен.

Успешный `ApplyCustomerAccess` на момент обрыва уже зафиксирован, так что
теряется максимум ответ вызывающему: точный повтор команды вернёт эквивалентное
состояние.

Переменные окружения и порядок наката миграций описаны в
[DEPLOYMENT.md](DEPLOYMENT.md).
