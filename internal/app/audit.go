package app

// Контракт журнала аудита.
//
// Значения собраны в одном файле, а не рядом с use case'ами, которые их пишут,
// по той же причине, по которой собран словарь кодов исхода nodeagent: по ним
// строятся запросы к audit_events и alert'ы, то есть это контракт наблюдаемости,
// а переименование любого значения после выкатки ломает их молча.
//
// Нотация одна на все значения — SCREAMING_SNAKE_CASE, как у остальных
// протокольных значений репозитория (статусы операций, apply_state, коды исхода
// агента, причины карантина). Первый аудит был написан в точечной нотации
// (`manifest.applied`), второй — в этой; расхождение устранено до первого push.

// Типы актора. Совпадают с ролями, приведёнными к общей нотации; SYSTEM
// означает фоновый воркер, за которым не стоит вызывающий.
const (
	auditActorTypeSystem         = "SYSTEM"
	auditActorTypeManifestWriter = "MANIFEST_WRITER"
	auditActorTypeAccessWriter   = "CUSTOMER_ACCESS_WRITER"
)

// Действия.
//
// Команда customer разложена на три действия, а не сведена к одному с полем в
// метаданных: Apply и renewal журналируются раздельно, и фильтр по колонке
// action дешевле и надёжнее разбора jsonb.
const (
	auditActionManifestApplied      = "MANIFEST_APPLIED"
	auditActionCustomerExpired      = "CUSTOMER_EXPIRED"
	auditActionCustomerCreated      = "CUSTOMER_CREATED"
	auditActionCustomerRenewed      = "CUSTOMER_RENEWED"
	auditActionCustomerQuotaChanged = "CUSTOMER_QUOTA_CHANGED"
)

// Типы цели.
const (
	auditTargetTypeManifest = "MANIFEST_REVISION"
	auditTargetTypeCustomer = "CUSTOMER"
)

// Исходы. Пока единственный: журналируются принятые изменения, а отклонённая
// команда откатывает свою транзакцию вместе с записью журнала.
const auditOutcomeAccepted = "ACCEPTED"

// AuditEvent — запись журнала аудита.
//
// Metadata обязана быть sanitized: ни секретов, ни client_uuid, ни accounting ID.
// Тип не проверяет это за вызывающего — проверять содержимое map он не может, —
// но фиксирует требование в одном месте.
type AuditEvent struct {
	ActorType  string
	ActorID    string
	Action     string
	TargetType string
	TargetID   string
	RequestID  string
	Outcome    string
	Metadata   map[string]any
}
