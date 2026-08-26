# Тестирование

Документ отвечает на вопрос, как устроены тесты репозитория: чем отбираются
интеграционные, откуда берётся чистая база, как написаны двойники портов и куда
класть новый тест.

Команды `make` перечислены в `README.md`, разделы «Разработка» и
«Тестирование». Схема базы разобрана в [DATABASE.md](DATABASE.md), состав
воркеров в [ARCHITECTURE.md](ARCHITECTURE.md), переменные окружения самого
процесса в [DEPLOYMENT.md](DEPLOYMENT.md). У тестов свои переменные, они ниже, и
с конфигурацией процесса не пересекаются.

## Состав

487 тестов в 68 файлах, одиннадцать пакетов.

| пакет | файлов | тестов | нужна база |
|---|---|---|---|
| `internal/postgres` | 15 | 109 | да, кроме десяти |
| `internal/app` | 13 | 99 | нет |
| `internal/domain` | 13 | 83 | нет |
| `internal/grpcsvc` | 3 | 47 | нет |
| `cmd/spiritvpnd` | 7 | 39 | да, у шести |
| `internal/crypto` | 5 | 32 | нет |
| `internal/nodeagent` | 3 | 32 | нет |
| `internal/metrics` | 6 | 27 | нет |
| `cmd/migrate` | 1 | 9 | нет |
| `internal/config` | 1 | 8 | нет |
| `internal/migrations` | 1 | 2 | нет |

Десять тестов `internal/postgres`, обходящихся без базы, лежат в
`internal/postgres/node_public_test.go`, `numeric_test.go`, `links_test.go` и
`available_nodes_test.go`: разбор `public_config`, преобразование
`numeric(20,0)` в `uint64` и маппинг строк read-моделей.

Библиотек тестирования в `go.mod` нет: ни testify, ни генератора моков, ни
sqlmock. Сравнения написаны на `if` с `t.Fatalf` и `t.Errorf`, двойники портов
собраны руками. Единственная сторонняя обвязка это
`prometheus/client_golang/prometheus/testutil` в `internal/metrics`: `ToFloat64`
читает значение конкретной серии в 40 местах, `GatherAndLint` вызывается один
раз в `internal/metrics/registry_test.go:38`: он собирает весь реестр и
отбирает замечания по своему namespace, новую метрику подхватит сам. Соседний
`TestNoCustomerIDLabels` (строка 57) устроен обратным образом, на белом списке
меток; заводя метку, впишите её туда, иначе тест упадёт.

Каталогов `testdata` в репозитории нет. Зафиксированный эталон один:
`goldenDigest` в `internal/domain/manifest_digest_test.go:17`, шестнадцатеричная
строка прямо в исходнике. Правка канонического представления манифеста валит
`TestCanonicalizeManifestGolden` и только его: остальные тесты дайджеста
(`internal/postgres/manifest_integration_test.go:142`,
`internal/app/apply_fleet_manifest_test.go:143`) вычисляют ожидаемое значение той
же функцией и остаются зелёными.

## Гейт интеграционных тестов

Гейт это пара переменных окружения `SPIRITVPN_INTEGRATION_TESTS` и
`DATABASE_URL`. Реализаций две, и при неполном наборе они ведут себя по-разному.

| набор переменных | `TestMain` (`internal/postgres/apply_integration_test.go:39`) | `newSchemaPool` (`cmd/spiritvpnd/schema_integration_test.go:33`) |
|---|---|---|
| обе заданы | накатывает миграции, гоняет все тесты пакета | пересоздаёт свою схему, гоняет тест |
| `SPIRITVPN_INTEGRATION_TESTS` пуст | сразу `m.Run()`; проходят шесть тестов без базы | `t.Skip` на строке 37 |
| он задан, `DATABASE_URL` пуст | сообщение в stderr и `os.Exit(1)` | `t.Fatal` на строке 44 |

Разница видна в последней строке. `os.Exit(1)` валит пакет `internal/postgres`
до первого теста, `t.Fatal` роняет только вызвавший тест. Забыв `DATABASE_URL`,
вы получите либо одну строку про пустой DSN и ни одного результата теста, либо
шесть падений в `cmd/spiritvpnd` при зелёном остальном прогоне.

`testing.Short()` не вызывает ни один тест репозитория. Цель `test-unit`
передаёт `-short`, и набор от этого флага не меняется: `go test -short ./...` и
`go test ./...` без заданных переменных отбирают одно и то же. Офлайновость
обычного прогона держится на гейте.

Приставка `TestIntegration` в имени это второй механизм отбора: цель
`test-integration` передаёт `-run Integration`. Функций с такой приставкой 102,
из них 96 в `internal/postgres` и 6 в
`cmd/spiritvpnd/schema_integration_test.go`, поэтому прогон захватывает два
пакета. Тест, которому нужна база, но названный иначе, в этот прогон не попадёт,
а в обычном уйдёт в `t.Skip`, и не выполнится нигде.

## База интеграционных тестов

`docker-compose.dev.yml` поднимает отдельный экземпляр PostgreSQL, не связанный с
`docker-compose.yml`.

| параметр | значение | следствие |
|---|---|---|
| образ | `postgres:14-alpine` | та же мажорная версия, что в `services:` у `.github/workflows/ci.yml` |
| порт | 5433 | подключение по привычке на 5432 попадёт мимо этой базы |
| данные | `tmpfs` на `/var/lib/postgresql/data` | содержимое умирает вместе с контейнером |
| `fsync`, `synchronous_commit`, `full_page_writes` | `off` | сохранять здесь нечего, прогон быстрее |
| healthcheck | `pg_isready` | `docker compose up --wait` дожидается готовности |

Схему накатывает `migrateUp` (`internal/postgres/apply_integration_test.go:60`)
пакетом `internal/migrations`, тем же, что и команда деплоя. Тесты идут по той
схеме, из которой sqlc сгенерировал код в `internal/postgres/db`.

Перед каждым тестом `newFixture` выполняет `TRUNCATE ... CASCADE` по списку
`truncatedTables` (`internal/postgres/apply_integration_test.go:90`). Список
явный, 15 таблиц, в порядке, обратном зависимостям. Заводя таблицу в схеме,
допишите её сюда: без строки в списке данные предыдущего теста доедут до
следующего и дадут падение, не связанное с проверяемым кодом.

Тесты `cmd/spiritvpnd` работают в отдельной схеме
`spiritvpnd_schema_check_test`. `newSchemaPool` прописывает её в `search_path`
соединения и пересоздаёт на каждый вызов вместе с собственной таблицей
`schema_migrations`. Проверяемая функция `schemaCheck` обращается к
`schema_migrations` по захардкоженному имени, и `search_path` уводит запрос в
таблицу теста. Тесты двигают версию схемы и поднимают флаг `dirty`; настоящую
`schema_migrations` в это время использует `internal/postgres`, чьи тесты идут
параллельным процессом того же `go test ./...`.

## Фикстуры

Фикстура это функция, отдающая чистую базу вместе с use case, собранным на
настоящих адаптерах. Корень один: `newFixture`
(`internal/postgres/apply_integration_test.go:109`). Он проверяет гейт, при
пустой переменной делает `t.Skip` на строке 113, открывает пул, чистит таблицы и
возвращает `ApplyCustomerAccess` вместе с `*pgxpool.Pool`. Генератор
идентификаторов и шифр внутри настоящие: `crypto.NewGenerator` и `testCipher`
(строка 136) на детерминированном ключе.

Остальные шесть функций вызывают корень и надстраивают use case поверх того же
пула и того же шифра. Все они возвращают структуру целиком, поля перечислены
рядом с объявлением.

| функция | место | состав |
|---|---|---|
| `newManifestFixture` | `internal/postgres/manifest_integration_test.go:22` | `ApplyFleetManifest` |
| `newLinksFixture` | `internal/postgres/links_integration_test.go:28` | `ApplyCustomerAccess` и `GetCustomerAccessLinks` |
| `newMaterializeStack` | `internal/postgres/materialize_integration_test.go:32` | манифест, customer, материализация, ссылки |
| `newDispatchStack` | `internal/postgres/dispatch_integration_test.go:105` | то же плюс диспетчер и поддельный агент; TTL lease берёт параметром |
| `newExpiryStack` | `internal/postgres/expiry_integration_test.go:32` | состав `newDispatchStack` плюс истечение сроков |
| `newUsageStack` | `internal/postgres/usage_integration_test.go:89` | манифест, customer, материализация, ссылки, диспетчер, сбор трафика, два разных поддельных агента и буфер логов |

Второй этаж один: `newInventoryStack`
(`internal/postgres/reconcile_integration_test.go:360`) принимает готовый
`usageStack` параметром и добавляет к нему `ReconcileNodes`.

Буфер логов в `usageStack` собирает всё, что воркеры написали за прогон, целиком.
По нему работает `TestIntegrationNoSecretsInLogs`
(`internal/postgres/secrets_integration_test.go:79`): добавив в воркер новую
строку лога с полем из credential, вы уроните именно этот тест.

Чистую базу даёт только корень лестницы. Новый интеграционный тест начинайте с
одной из перечисленных функций; собственный `pgxpool.New` получит пул без
`TRUNCATE`, то есть остатки предыдущего теста.

## Двойники портов

Двойник это подставная реализация порта app-слоя, написанная в тестовом файле.
Одиннадцать файлов `internal/app` объявляют пакет `app_test`, внешний по
отношению к `app`, и видят только экспортированное. Внутренний файл один,
`internal/app/prune_usage_dedup_test.go` с `package app`.

Устройство двойника видно на `fakeDispatchRepo`
(`internal/app/dispatch_operations_test.go:24`). Помимо заготовленных ответов и
ошибок он ведёт поле `journal`: каждый метод дописывает туда своё имя, а
`WithinResultTx` (строка 64) обрамляет вызов записями `tx-begin` и `tx-commit`.
Один тип реализует и `DispatchRepository`, и `ResultTx`, поэтому по вызову
`SetAccessApplyState` не видно, внутри транзакции он сработал или снаружи;
видно это только по положению записи между `tx-begin` и `tx-commit`. Двойник
агента пишет своё `rpc` в тот же журнал (строка 173 передаёт ему указатель на
поле), и проверка `equalSteps` (строка 222) сравнивает всю последовательность
целиком. Так фиксируется, что доставка на ноду идёт вне транзакции записи
результата.

Не все зависимости в этих тестах подставные. `testSealer`
(`internal/app/dispatch_operations_test.go:136`) собирает настоящий
`crypto.Cipher` на детерминированном ключе вместо заглушки, которой пользуются
соседние тесты. Тест сверяет `client_uuid`, уехавший на ноду, с тем, что был
запечатан; заглушка с подменённым `Open` вернула бы заготовленное значение и эту
сверку бы сняла.

## Тесты домена

`internal/domain` это 83 теста в 13 файлах, без базы и без двойников: функции
пакета чистые, вход и выход у них значения.

Фикстуры общие на весь пакет. Тесты объявляют `package domain`, и конструктор,
написанный в одном файле, работает во всех остальных: `exampleTopology`
(`internal/domain/access_test.go:21`) и `materializedAccesses`
(`internal/domain/apply_test.go:13`) собирают вход тестам материализации из
третьего файла. Разыскивая, откуда взялось значение, ищите по всему пакету.

Преобладающая форма это конструктор согласованного входа рядом с хелпером,
зовущим планировщик и валящим тест на ошибке. `materializeInput`
(`internal/domain/materialize_test.go:15`) с `planOrFailMaterialize` (строка 30);
той же парой устроен `internal/domain/manifest_plan_test.go`, `projectionOf`
(строка 11) и `planOrFail` (строка 45). Тест поверх такой пары ломает ровно одно
условие базового входа: `TestPlanMaterializeAddsNode`
(`internal/domain/materialize_test.go:56`) дописывает ноду в топологию и в список
живых, остального не трогает. Новый случай пишется так же, собирать
`MaterializationInput` с нуля не нужно.

Табличная форма с `t.Run` встречается точечно: 14 вызовов на весь пакет, больше
всего в `internal/domain/command_test.go` (три). Четыре файла обходятся без неё
вовсе.

Соответствие адаптеров портам проверяется на этапе компиляции, одиннадцатью
объявлениями в трёх местах.

| место | что закреплено |
|---|---|
| `internal/postgres/expiry.go:159`, `internal/postgres/usage.go:445`, `internal/postgres/dispatch.go:188` | `*Repository` закрывает `ExpiryRepository`, `UsageRepository`, `DispatchRepository` |
| `internal/postgres/apply_integration_test.go:33` и `:34`, `internal/postgres/manifest_integration_test.go:20`, `internal/postgres/links_integration_test.go:26`, `internal/postgres/materialize_integration_test.go:19` | ещё четыре порта репозитория плюс `ApplyTx` на `*applyTx`; ссылка на порт живёт в тесте |
| `internal/app/ports_test.go:15-17` | `*crypto.Generator`, `*crypto.Cipher` и `app.SystemClock` закрывают `IDs`, `CredentialSealer` и `Clock` |

Сняв метод из интерфейса порта или переименовав его, вы получите ошибку
компиляции, а не упавший тест. Восемь объявлений из одиннадцати стоят в тестовых
файлах, и `go build ./...` их не увидит; ошибка вылезет на `go vet` или
`go test`.

## Рукопожатие mTLS

Границу mTLS проверяют с двух сторон, обе с настоящим TLS и настоящей проверкой
цепочки до CA.

| файл | что поднимается | что закрывается |
|---|---|---|
| `cmd/spiritvpnd/mtls_test.go` | продакшн-сборщик `newGRPCServer` (вызов на строке 296) слушает на `127.0.0.1:0`; `tls.RequireAndVerifyClientCert` приходит из `cmd/spiritvpnd/grpc.go:117` | 12 тестов: сопоставление URI SAN с ролью, отказ без клиентского сертификата, отказ по чужому CA, игнорирование Common Name, отсутствие выданной ссылки в логах |
| `internal/nodeagent/client_test.go` | поддельный агент (`startAgent`, строка 242), требующий и проверяющий клиентский сертификат backend | backend как клиент: разбор кодов ответа, постоянная ошибка при несовпадении идентичности, переиспользование соединения на ноду и его замена при смене endpoint |

База ни одному из этих файлов не нужна. В `cmd/spiritvpnd/mtls_test.go` за
use case стоят заглушки, `internal/nodeagent/client_test.go` до слоя хранения
вообще не доходит.

`internal/grpcsvc/interceptors_test.go` рукопожатия не делает. Хелпер
`tlsContext` (строка 45) кладёт в контекст готовый `peer.Peer` с уже разобранной
цепочкой сертификатов. 22 теста файла покрывают решение по идентичности и роли,
состав полей лога и отображение доменных ошибок в коды gRPC. Непокрытым остаётся
всё, что происходит до интерцептора: проверка цепочки, срок действия
сертификата, минимальная версия TLS. Испортив `ClientCAs` или `MinVersion` в
сборке сервера, вы увидите падение в `cmd/spiritvpnd/mtls_test.go`, а в
`internal/grpcsvc` ничего не изменится.

## Конкурентные тесты

Схем две, и они противоположны по ожиданию.

Первая: конкурирующая транзакция держит строку и не коммитится, испытуемый вызов
уходит в горутину, тест убеждается, что горутина не вернулась, снимает блокировку
и дожидается результата.

| тест | что держит конкурент | что обязано произойти |
|---|---|---|
| `TestIntegrationApplySerializesConcurrentCommands` (`internal/postgres/apply_integration_test.go:634`) | `SELECT ... FOR UPDATE` на корневой строке customer | вторая команда ждёт, `last_command_number` во время ожидания не двигается |
| `TestIntegrationDispatchRefusesSecondInFlightOnNode` (`internal/postgres/dispatch_integration_test.go:277`) | перевод операции в `IN_FLIGHT` без commit | `LeaseNext` не берёт вторую операцию той же ноды и возвращается без ошибки |

Вторая схема обратная. `TestIntegrationExpirySkipsLockedCustomer`
(`internal/postgres/expiry_integration_test.go:241`) держит строку единственного
готового к истечению customer и требует, чтобы воркер вернулся немедленно и с
пустыми руками. Ожидание дольше пяти секунд валит тест: так выглядит `FOR UPDATE`
без `SKIP LOCKED` в запросе.

Тест на конкуренцию легко написать так, что он останется зелёным и после снятия
проверяемой защиты. В `TestIntegrationDispatchRefusesSecondInFlightOnNode` от
этого сделан отдельный шаг (строка 289):

```sql
UPDATE agent_operations SET next_attempt_at = now() + interval '1 hour' WHERE node_id <> $1
```

Он убирает из выборки операции всех прочих нод. Без него `ORDER BY` отдаёт
глобально первую готовую операцию, она оказывается на свободной ноде,
конкуренции за занятую не возникает, и тест проходит при снятом частичном
уникальном индексе. Написав такой тест, снимите проверяемую защиту и убедитесь,
что он падает.

## Покрытие

Профиль снимает шаг `Run tests` (`.github/workflows/ci.yml:70`):
`go test -race -coverprofile=coverage.out -covermode=atomic ./...`.
Интеграционные тесты посчитаны в этой же job: у неё объявлен `services: postgres`
и обе переменные гейта заданы на уровне job.

Порог 80 проверяет шаг `Check coverage threshold`. Он стоит после выгрузки
`coverage.html` артефактом; переставив его выше, вы потеряете отчёт ровно в тех
прогонах, где покрытие просело. Шаг разбирает вывод `go tool cover -func` и
отдельно проверяет, что процент вообще разобрался: пустая строка валит шаг, а не
проходит его молча.

Число зависит от способа счёта. По умолчанию каждый пакет считается по себе, код
`internal/domain` засчитывается только тестами `internal/domain`. С
`-coverpkg=./...` в знаменатель попадают все пакеты, включая покрываемые чужими
тестами, и итог получается заметно ниже. Порог настроен на первый способ, тот,
что в команде выше; сравнивать локальный прогон с CI имеет смысл при совпадающем
наборе флагов и при поднятой базе.

## Размещение нового теста

| что проверяется | пакет | приставка имени | нужна база |
|---|---|---|---|
| правило домена, план по заданному входу | `internal/domain` | `Test` | нет |
| порядок вызовов портов, ветки их ошибок | `internal/app` | `Test` | нет |
| SQL: блокировки, индексы, порядок строк, типы колонок | `internal/postgres` | `TestIntegration` | да |
| преобразование protobuf в домен, коды ошибок | `internal/grpcsvc` | `Test` | нет |
| решение по идентичности и роли, состав полей лога | `internal/grpcsvc` | `Test` | нет |
| разговор с агентом по gRPC, классификация ответов | `internal/nodeagent` | `Test` | нет |
| сборка процесса, mTLS, завершение, запуск воркеров | `cmd/spiritvpnd` | `Test` | нет |
| проверка готовности по `schema_migrations` | `cmd/spiritvpnd` | `TestIntegration` | да |
| имена, метки и заполнение метрик | `internal/metrics` | `Test` | нет |
| разбор переменных окружения | `internal/config` | `Test` | нет |
| встроенный набор миграций и его версия | `internal/migrations` | `Test` | нет |
| команда наката схемы | `cmd/migrate` | `Test` | нет |

Тест из строк с базой начинается с фикстуры (`internal/postgres`) или с
`newSchemaPool` (`cmd/spiritvpnd`) и обязан называться с `TestIntegration`. При
другом имени `-run Integration` его не запустит, а обычный прогон уведёт в
`t.Skip`, и тест не выполнится ни в одном режиме.
