# Внешний API

Внешних поверхностей две, обе gRPC и обе под mTLS. `CustomerAccessService`
принимает команды product-сервиса, отдаёт ссылки и публичный каталог нод.
`ManifestService` принимает манифест топологии от infrastructure CI/CD. Все
шесть методов unary, стриминга нет.

Контракт задают `.proto`; сгенерированный код лежит в `internal/gen` и
в репозиторий закоммичен. Этот документ отвечает на вопрос, что видно снаружи;
путь запроса через код разобран в [ARCHITECTURE.md](ARCHITECTURE.md).

## Поверхности

| сервис | метод | роль | вызывающий |
|---|---|---|---|
| `CustomerAccessService` | `ApplyCustomerAccess` | `customer-access-writer` | product-сервис |
| `CustomerAccessService` | `GetCustomerAccessLinks` | `customer-access-reader` | product-сервис |
| `CustomerAccessService` | `ListAvailableNodes` | `customer-access-reader` | product-сервис |
| `CustomerAccessService` | `SetCustomerAccessState` | `customer-access-admin` | административный сервис |
| `CustomerAccessService` | `DeleteCustomerAccess` | `customer-access-admin` | административный сервис |
| `ManifestService` | `ApplyFleetManifest` | `manifest-writer` | infrastructure CI/CD |

Файлы контракта: `proto/spiritvpn/customer/v1/customer.proto` и
`proto/spiritvpn/manifest/v1/manifest.proto`. Сервисы регистрируются в
`cmd/spiritvpnd/grpc.go:78`, адрес прослушивания задаёт `SPIRIT_GRPC_LISTEN`
(по умолчанию `:8443`).

Служебный порт `SPIRIT_HTTP_LISTEN` (по умолчанию `:8080`) несёт `/health/live`,
`/health/ready` и `/metrics`, `cmd/spiritvpnd/health.go:32`. Ни TLS, ни проверки
вызывающего у него нет: кто дотянулся до порта, тот всё и прочитал. Ограничивать
доступ приходится сетью, наружу порт не публикуется.

Третий `.proto` в дереве, `proto/spiritvpn/nodeagent`, вендорен из репозитория
агента. По нему backend ходит клиентом к нодам поверх management-overlay; снаружи
эта поверхность не видна и здесь не рассматривается.

## mTLS и роли

mTLS-идентичность это строка из SAN клиентского сертификата, по которой backend
узнаёт вызывающего. `peerIdentities` (`internal/grpcsvc/auth.go:129`) берёт
листовой сертификат проверенной цепочки и читает два поля SAN: `DNSNames` и
`URIs`. Годятся значения вида `product-svc.internal` и
`spiffe://spiritvpn/product`.

У сертификата этих значений может быть несколько, тогда вызывающему достаточно,
чтобы в списке роли нашлось любое из них. Поле CN не читается вовсе: сертификат,
у которого имя есть только в CN, идентичности не имеет и получит
`UNAUTHENTICATED`.

Транспорт собирает `transportCredentials`, `cmd/spiritvpnd/grpc.go:90`:
`MinVersion` равен TLS 1.3, `ClientAuth` равен `RequireAndVerifyClientCert`, пул
`ClientCAs` читается из `SPIRIT_GRPC_TLS_CLIENT_CA_FILE`. Insecure-режима нет ни в
одной сборке; для локальной разработки те же самые сертификаты выпускает
`make dev-certs`.

Списки разрешённых идентичностей приходят из окружения:

| роль | переменная |
|---|---|
| `customer-access-writer` | `SPIRIT_ROLE_CUSTOMER_ACCESS_WRITER` |
| `customer-access-reader` | `SPIRIT_ROLE_CUSTOMER_ACCESS_READER` |
| `customer-access-admin` | `SPIRIT_ROLE_CUSTOMER_ACCESS_ADMIN` |
| `manifest-writer` | `SPIRIT_ROLE_MANIFEST_WRITER` |

Значение это список через запятую; пустые элементы отбрасываются, лишняя запятая
безвредна. Если пусты все четыре переменные, процесс падает на старте.

Роли независимы, иерархии между ними нет. Идентичность, перечисленная только в
`SPIRIT_ROLE_CUSTOMER_ACCESS_WRITER`, на `GetCustomerAccessLinks` получит
`PERMISSION_DENIED`. Product-сервису, который вызывает команду и оба read-метода,
свою идентичность нужно вписать в обе переменные, одной и той же строкой.

Таблица `methodRoles` (`internal/grpcsvc/auth.go`) содержит ровно шесть строк, по
одной на метод. Метод, которого в ней нет, запрещён всем: `authorize` отвечает
`PERMISSION_DENIED`, не доходя до хендлера. В тексте отказа не сказано, какой
роли не хватило, так что искать причину придётся по `peer_identity` в логе.

### Interceptor'ы

Цепочка собрана в `cmd/spiritvpnd/grpc.go:71` в порядке request_id → логирование →
авторизация. Логирование стоит перед авторизацией, и отказ авторизации попадает в
лог вместе с идентичностью, которой отказали.

`x-request-id` берётся из метадаты запроса, `internal/grpcsvc/requestid.go:38`.
Пригодно значение длиной от 1 до 64 байт из печатных ASCII без пробелов.
Непригодное молча заменяется сгенерированным UUID, и запрос от этого не
отклоняется.

На каждый вызов пишется одна запись, `internal/grpcsvc/logging.go:22`, с полями
`method`, `grpc_code`, `error_code`, `duration_ms`, `request_id`, `peer_identity`.
Тело запроса и ответ не логируются никогда: в запросе `customer_id`, в ответе URI
с credentials. Уровень записи выбирает `levelFor`: `OK` даёт info, `INTERNAL`,
`UNKNOWN`, `DATA_LOSS` и `UNAVAILABLE` дают error, остальные исходы warn.

## ApplyCustomerAccess

Одна команда product-сервиса. Отдельного поля «что сделать» в ней нет: род
команды backend выводит сам, сравнивая присланный `expires_at` с сохранённым.

| поле | тип | ограничение |
|---|---|---|
| `customer_id` | `string` | непустой, не длиннее 256 байт; для backend непрозрачен |
| `vpn_fleet_id` | `int64` | > 0, присутствует в текущем манифесте |
| `usage_quota_bytes` | `uint64` | > 0 |
| `expires_at_epoch_sec` | `int64` | UTC Unix timestamp в секундах |
| `command_number` | `uint64` | > 0, монотонно возрастает по customer |

Квота задаётся на каждую ноду отдельно, не на флот целиком: customer с
`usage_quota_bytes` в 100 ГБ на флоте из трёх нод израсходует до 100 ГБ на каждой
из них. Перевод из GB или GiB в байты делает product-сервис до вызова.

`expires_at_epoch_sec` это абсолютный момент окончания доступа, не длительность.
Точность секундная, доли секунды передать нечем.

Род команды определяет `domain.ClassifyApply`
(`internal/domain/command.go:91`):

| условие | род команды | что при этом происходит |
|---|---|---|
| у customer ещё нет сохранённого срока | создание | момент обязан быть в будущем, открывается первый период квоты |
| присланный срок больше сохранённого | продление | момент обязан быть в будущем, открывается новый период квоты, счётчики трафика обнуляются |
| присланный срок равен сохранённому | смена квоты | момент в будущем не требуется, счётчики трафика сохраняются |
| присланный срок меньше сохранённого | отказ | `EXPIRY_REGRESSION` |

Отсюда следует правило для повторной отправки: тот же `expires_at_epoch_sec` до
секунды означает смену квоты, значение хоть на секунду больше означает продление
со сбросом трафика.

Смена `vpn_fleet_id` у существующего customer не поддерживается и отвечает
`FLEET_MISMATCH`.

`domain.ValidateApplyCommand` проверяет форму запроса до открытия транзакции:
`customer_id` непуст, `vpn_fleet_id`, `usage_quota_bytes` и `command_number`
больше нуля. Всё остальное, включая присутствие флота в манифесте и правила
вокруг `expires_at`, проверяется внутри транзакции.

### Идемпотентность

Apply, административное состояние и удаление образуют общий поток команд по
customer. Меньший `command_number` поглощается как reorder. Равный номер с тем же
SHA-256 fingerprint является идемпотентным повтором; равный номер с другим RPC
или payload возвращает `COMMAND_NUMBER_CONFLICT`. Проверка выполняется под
`SELECT ... FOR UPDATE`, и повтор коммитит пустую транзакцию.

Проверка идёт раньше, чем backend смотрит, есть ли флот в манифесте. Повтор
старой команды для флота, успевшего из манифеста исчезнуть, получает `OK`,
а не `FLEET_NOT_FOUND`.

У нового customer сохранённого номера нет, его первая команда принимается с любым
`command_number > 0`.

### Ответ

`ApplyCustomerAccessResponse` пуст. Успех означает, что desired state и строки
`agent_operations` зафиксированы в PostgreSQL. Юзера на ноде в этот момент ещё
нет: доставку ведёт диспетчер асинхронно, и наблюдать за ней вызывающий может
через `GetCustomerAccessLinks`.

Тот же пустой ответ приходит на поглощённый повтор. Снаружи принятие команды и
идемпотентный no-op неразличимы.

Для `BLOCKED` customer Apply меняет срок и квоту, но не снимает
административный блок. Для `DELETING` новый Apply возвращает
`FAILED_PRECONDITION`. Для `DELETED` Apply с большим номером создаёт новый
период, access и credentials; старые URI не восстанавливаются.

## GetCustomerAccessLinks

Запрос несёт один `customer_id`. В ответ уходят все текущие ссылки этого
customer, без пагинации, в порядке `(kind, logical_target_key, access_id)`.
Ретайрнутые access, то есть отозванные при исчезновении цели из манифеста, в
ответ не попадают, как и цели, которых в текущем манифесте нет. Неизвестный
`customer_id` даёт `NOT_FOUND` (`internal/postgres/links.go:44`).

Состояние каждой ссылки выводит `domain.LinkStatusOf`
(`internal/domain/link.go:79`) на каждый ответ; нигде оно не хранится.

| поле | присутствие | значение |
|---|---|---|
| `kind` | всегда | `FREEDOM` либо `BRIDGE` |
| `state` | всегда | текущее состояние ссылки |
| `block_reason` | только `BLOCKED` | причина блокировки |
| `uri` | только `READY` | готовая VLESS URI |
| `usage_quota_bytes` | всегда | лимит текущего периода на входной ноде |
| `consumed_bytes` | всегда | учтённый расход текущего периода на входной ноде |

| `state` | условие | `block_reason` | `uri` |
|---|---|---|---|
| `BLOCKED` | текущее время базы достигло `expires_at` | `TIME_EXPIRED` | нет |
| `BLOCKED` | исчерпана квота на входной ноде | `TRAFFIC_QUOTA_EXHAUSTED` | нет |
| `FAILED` | агент ответил так, что повтор не поможет, либо параметры входной ноды непригодны | нет | нет |
| `READY` | desired state `PRESENT`, агент подтвердил | нет | есть |
| `PENDING` | всё остальное | нет | нет |

Порядок строк повторяет порядок ветвей функции и задаёт приоритет причин: когда
применимы и срок, и квота, наружу уходит `TIME_EXPIRED`.

Поля `block_reason` и `uri` объявлены `optional`, и читать их следует по
состоянию: у `READY` всегда есть `uri`, у `BLOCKED` всегда есть `block_reason`, в
остальных случаях полей нет физически. Проверка на пустую строку вместо проверки
состояния работать не будет. Заполняет их `linkTo`
(`internal/grpcsvc/customer_access.go:152`).

`usage_quota_bytes` и `consumed_bytes` также объявлены `optional`, но присутствуют
при любом состоянии возвращённой ссылки. Они относятся к входной ноде access:
для FREEDOM это сама нода, для BRIDGE — `entry_node_id` связи. Несколько ссылок с
общей входной нодой поэтому возвращают одинаковый суммарный расход.
`consumed_bytes` содержит уже учтённые backend данные и может отставать от
фактического трафика на интервал опроса node-agent.

| `kind` | нода для quota-полей |
|---|---|
| `FREEDOM` | целевая нода ссылки; она же `vpn_accesses.entry_node_id` |
| `BRIDGE` | входная нода связи (`entry_node_id`), не exit-нода |

Остаток для отображения или расчёта продления конкретной ссылки следует считать
как `max(usage_quota_bytes - consumed_bytes, 0)`. Обычное беззнаковое вычитание
небезопасно: node-agent отдаёт накопленный трафик порциями, поэтому после
очередного сбора `consumed_bytes` может оказаться больше лимита.

Ответ, в котором часть ссылок `READY`, а часть `PENDING` или `FAILED`, штатен и
ошибкой не является. Отказ расшифровать credential и непригодные публичные
параметры ноды гасят до `FAILED` одну ссылку, остальные ссылки того же customer
остаются как были. Разбор самой URI в [VLESS.md](VLESS.md).

### Кеширование

Ответ несёт метадату `cache-control: no-store`. Это метадата gRPC, а не
HTTP-заголовок: никакой промежуточный слой её не исполнит, и не класть такой
ответ в кеш обязан сам вызывающий. Ставится метадата до вызова use case
(`internal/grpcsvc/customer_access.go:100`) и приходит на любом ответе метода,
включая ошибку.

Внутри `uri` лежит расшифрованный `client_uuid`, то есть credentials доступа.
Backend такой ответ не логирует; на стороне вызывающего он не должен попадать в
логи, трассировки и кеши.

## ListAvailableNodes

Запрос пуст и не зависит от `customer_id`: метод предназначен для каталога до
покупки. Ответ группирует актуальные ноды infrastructure manifest по fleet:

| поле | значение |
|---|---|
| `fleets[].vpn_fleet_id` | идентификатор fleet |
| `fleets[].nodes[].node_id` | стабильная идентичность ноды из manifest |
| `fleets[].nodes[].display_name` | `ManifestNode.display_name` |

В выборку входят только строки, для которых одновременно актуальны fleet,
membership и сама нода (`current = true`). Пустые fleets не возвращаются. Одна
нода, состоящая в нескольких fleets, присутствует в каждом из них. Порядок
детерминирован: fleet по `vpn_fleet_id`, ноды внутри него по `node_id`.

«Доступная» здесь означает «присутствует в актуальном manifest». Метод не читает
customer access и квоты, не учитывает BRIDGE-связи и не обращается к node-agent,
поэтому временная недоступность агента список не меняет. В ответе нет
credentials; правило `cache-control: no-store` метода ссылок на него не
распространяется.

## SetCustomerAccessState

Административная команда принимает `customer_id`, общий `command_number` и
состояние `ACTIVE` либо `BLOCKED`. Переход в `BLOCKED` сохраняет подписку, квоту
и credentials и переводит в `ABSENT` все поколения access. `ENSURE_ABSENT`
выпускается для каждой входной ноды, которая есть в актуальном manifest;
исторические access исчезнувших нод считаются применёнными логически. Переход в
`ACTIVE` возвращает `PRESENT` только текущим access, где ещё действуют срок и
node quota.

Ответ подтверждает commit desired state, а не завершение RPC к нодам. В
`GetCustomerAccessLinks` административно заблокированные ссылки имеют
`BLOCKED/ADMINISTRATIVE_BLOCK`; во время удаления —
`BLOCKED/DELETION_IN_PROGRESS`. `DELETED` customer читается как `NOT_FOUND`.
Из `DELETING` и `DELETED` обычная разблокировка запрещена: полностью удалённого
customer возвращает только `ApplyCustomerAccess`.

## DeleteCustomerAccess

Первый вызов переводит существующего customer в `DELETING`, повышает версии всех
access и ставит durable `ENSURE_ABSENT` на входные ноды актуального manifest.
Для исчезнувших нод физический RPC невозможен, поэтому их historical access
закрываются логически и тоже участвуют в cleanup. Ответ `PENDING` означает, что
cleanup ещё не завершён. Повтор с тем же номером безопасен и возвращает текущее
состояние. Для неизвестного customer сразу создаётся `DELETED` tombstone и
возвращается `COMPLETED`, чтобы запоздавший старый Apply не создал его.

Worker `finalize-deletion` ждёт подтверждённого `ABSENT`, отсутствия актуальных
операций и окончания окна позднего usage, затем атомарно удаляет access,
операции, quota periods и traffic usage. В `customer_entitlements` остаётся
tombstone с `customer_id`, последним command token и `deleted_at`.
Append-only `audit_events` также сохраняются согласно политике аудита; под
«полным удалением» API понимает operational access/quota/usage данные, а не
стирание ordering token и журнала действий.

`FAILED_PERMANENT` или недоступная актуальная нода оставляют customer в
`DELETING`: backend не сообщает `COMPLETED`, пока отсутствие не подтверждено.
Новый Delete с большим номером выпускает новую проверочную `ENSURE_ABSENT`.

## ApplyFleetManifest

Запрос это манифест целиком: `schema_version`, `revision`, `allow_destructive`,
полный инвентарь нод и полный инвентарь флотов. Частичных обновлений нет, манифест
применяется целиком либо отвергается целиком.

| поле ответа | значение |
|---|---|
| `applied_revision` | номер ревизии, ставшей текущей; при успехе совпадает с присланным |
| `result` | `APPLIED` либо `IDEMPOTENT` |

`IDEMPOTENT` означает повтор той же revision с тем же каноническим digest. Здесь,
в отличие от `ApplyCustomerAccess`, идемпотентный повтор различим по ответу.

Ответ возвращается после того, как манифест записан в таблицы. Доступы customer
под новую топологию к этому моменту ещё не пересчитаны: пересчёт идёт следом
отдельной джобой, [ARCHITECTURE.md](ARCHITECTURE.md), раздел «Материализация».

Формат манифеста, ограничения полей, destructive-guard и все коды `MANIFEST_*`
разобраны в [MANIFEST.md](MANIFEST.md).

## Коды ошибок

Стабильный код ошибки это строка в поле `error_code` записи лога. Вызывающему она
не видна, наружу уходит только gRPC-статус с кодом и текстом. По стабильным кодам
строятся алерты, и переименование любого из них меняет контракт наблюдаемости.

Наружу уходит только текст из таблиц `internal/grpcsvc/errors.go`. Собственный
текст ошибки не уходит никогда, и разбирать причину по строке сообщения
вызывающему нечем: опираться следует на gRPC-код.

### Доменные

| стабильный код | gRPC-код | метод | текст наружу |
|---|---|---|---|
| `INVALID_CUSTOMER_ID` | `INVALID_ARGUMENT` | customer-команды и `GetCustomerAccessLinks` | `customer_id должен быть непустым и не длиннее 256 байт` |
| `INVALID_FLEET_ID` | `INVALID_ARGUMENT` | `ApplyCustomerAccess` | `vpn_fleet_id должен быть > 0` |
| `INVALID_QUOTA` | `INVALID_ARGUMENT` | `ApplyCustomerAccess` | `usage_quota_bytes должен быть > 0` |
| `INVALID_COMMAND_NUMBER` | `INVALID_ARGUMENT` | customer-команды | `command_number должен быть > 0` |
| `EXPIRY_NOT_IN_FUTURE` | `INVALID_ARGUMENT` | `ApplyCustomerAccess` | `expires_at должен быть в будущем` |
| `CUSTOMER_NOT_FOUND` | `NOT_FOUND` | `GetCustomerAccessLinks`, `SetCustomerAccessState` | `customer не найден` |
| `FLEET_NOT_FOUND` | `NOT_FOUND` | `ApplyCustomerAccess` | `fleet не найден` |
| `FLEET_MISMATCH` | `FAILED_PRECONDITION` | `ApplyCustomerAccess` | `customer уже привязан к другому fleet` |
| `EXPIRY_REGRESSION` | `FAILED_PRECONDITION` | `ApplyCustomerAccess` | `сокращение expires_at не поддерживается` |
| `OPEN_PERIOD_MISSING` | `INTERNAL` | `ApplyCustomerAccess` | `внутренняя ошибка` |
| `INVALID_ADMINISTRATIVE_STATE` | `INVALID_ARGUMENT` | `SetCustomerAccessState` | `некорректное административное состояние` |
| `COMMAND_NUMBER_CONFLICT` | `ALREADY_EXISTS` | customer-команды | `command_number уже использован другой командой` |
| `CUSTOMER_DELETING` | `FAILED_PRECONDITION` | `ApplyCustomerAccess`, `SetCustomerAccessState` | `удаление customer ещё не завершено` |

`DeleteCustomerAccess` неизвестного customer идемпотентно создаёт tombstone;
`SetCustomerAccessState` неизвестного customer возвращает `CUSTOMER_NOT_FOUND`.

`OPEN_PERIOD_MISSING` это нарушение инварианта «ровно один период квоты с
`closed_at IS NULL`» на стороне backend, а не ошибка вызывающего. Наружу оно
уходит обезличенным `INTERNAL`, в логе отличимо по стабильному коду.

### Транспортные

| стабильный код | gRPC-код | когда |
|---|---|---|
| `OK` | `OK` | успех, включая поглощённый повтор команды |
| `UNAUTHENTICATED` | `UNAUTHENTICATED` | нет клиентского сертификата либо в нём нет пригодной идентичности |
| `PERMISSION_DENIED` | `PERMISSION_DENIED` | идентичность не в списке роли либо метода нет в `methodRoles` |
| `CANCELED` | `CANCELED` | вызывающий закрыл контекст |
| `DEADLINE_EXCEEDED` | `DEADLINE_EXCEEDED` | истёк дедлайн запроса |
| `INTERNAL` | `INTERNAL` | всё неопознанное |

Вызов без TLS и сертификат без пригодного SAN дают один и тот же
`UNAUTHENTICATED`. Различить их по ответу нельзя.

### Манифестные

Двенадцать кодов `MANIFEST_*` перечислены в [MANIFEST.md](MANIFEST.md). Их
gRPC-код говорит, чинить манифест или разбираться с историей ревизий.
`INVALID_ARGUMENT` означает, что не в порядке сам присланный манифест: не та
`schema_version`, дубль `node_id`, связь на несуществующую ноду.
`FAILED_PRECONDITION` означает, что манифест внутренне цел, но конфликтует с уже
принятым: ревизия младше текущей, тот же номер ревизии с другим содержимым,
пропавший флот, удаление без `allow_destructive`.

Только у этих кодов в тексте есть деталь: какая нода, какая связь, какое
значение. Деталь берётся из самого присланного манифеста.

### Порядок разбора

`statusFromError` (`internal/grpcsvc/errors.go:195`) проверяет ошибку в четыре
шага:

1. правила манифеста, `errors.As` до `*domain.ManifestValidationError`;
2. доменные сентинелы, `errors.Is` по таблице `errorMapping`;
3. `ctx.Err()` входящего контекста;
4. `INTERNAL` для всего остального.

`CANCELED` и `DEADLINE_EXCEEDED` вызывающий получает только тогда, когда закрылся
контекст его собственного вызова. Если контекст вызова жив, а `context.Canceled`
всё равно случилась где-то внутри backend, наружу уходит `INTERNAL`.

Доменные исходы идут раньше контекста. Отмена вызова, случившаяся сразу после
доменной ошибки, не подменяет `FAILED_PRECONDITION` на `CANCELED`.
