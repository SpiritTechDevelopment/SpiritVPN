package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	"github.com/RomanRyabinkin/SpiritVPN/internal/nodeagent"
)

// ApplyRepository открывает короткую транзакцию под одну команду customer.
//
// Транзакцией владеет адаптер (он знает про READ COMMITTED, commit и rollback), а
// порядок шагов внутри неё задаёт use case: именно порядок, а не отдельные запросы,
// является нормативным требованием §11.1, и держать его в SQL-слое значило бы
// сделать его непроверяемым без базы.
type ApplyRepository interface {
	// WithinTx выполняет fn в одной транзакции: nil коммитит, ошибка откатывает.
	WithinTx(ctx context.Context, fn func(ApplyTx) error) error
}

// ApplyTx — шаги одной транзакции ApplyCustomerAccess в порядке блокировок §11.1.
//
// Методы разделены по строкам, которые они блокируют, чтобы порядок вызовов в use
// case читался как нормативный список §11.1: entitlement → quota_periods →
// node_quota_usage → vpn_nodes → vpn_accesses → agent_operations. Последние три
// группы записываются одним WritePlan: они пишутся всегда вместе, и разбивать их
// значило бы дать вызывающему возможность нарушить порядок.
type ApplyTx interface {
	// Now — время начала транзакции (SELECT now()). Первый оператор транзакции;
	// единственный источник времени для доменных решений (решение 2, §11.1).
	Now(ctx context.Context) (time.Time, error)

	// LockEntitlement берёт SELECT ... FOR UPDATE корневой строки. nil означает
	// нового customer: строки ещё нет, и её создаст WritePlan.
	LockEntitlement(ctx context.Context, customerID string) (*domain.Entitlement, error)

	// FleetIsCurrent сообщает, присутствует ли fleet в последнем принятом
	// снапшоте manifest (§5, правило 6).
	FleetIsCurrent(ctx context.Context, fleetID int64) (bool, error)

	// LockOpenQuotaPeriod берёт FOR UPDATE единственный период с
	// closed_at IS NULL. nil означает, что открытого периода нет.
	LockOpenQuotaPeriod(ctx context.Context, customerID string) (*domain.QuotaPeriod, error)

	// LockNodeQuotaUsage берёт FOR UPDATE строки расхода периода в порядке node_id.
	LockNodeQuotaUsage(ctx context.Context, periodID uuid.UUID) ([]domain.NodeQuotaUsage, error)

	// LoadTopology читает текущую проекцию топологии fleet. Row locks не берёт.
	LoadTopology(ctx context.Context, fleetID int64) (domain.FleetTopology, error)

	// LoadAccesses читает ВСЕ access customer, включая ретайрнутые (§4). Row locks
	// не берёт: конкурирующий writer невозможен под lock корневой строки.
	LoadAccesses(ctx context.Context, customerID string) ([]domain.Access, error)

	// WritePlan записывает план целиком в порядке блокировок §11.1 и двигает
	// last_command_number. Вызывается и на пустом плане: валидный no-op номер
	// команды всё равно двигает (§5, правило 4).
	WritePlan(ctx context.Context, plan MaterializedPlan) error

	// AppendAudit добавляет запись в audit_events (§15).
	//
	// В той же транзакции, что и изменение, а не после неё: журнал, переживший
	// откат, утверждал бы про изменения, которых нет. Обратная сторона — отказ
	// команды не оставляет следа, и это осознанный предел аудита v1.
	AppendAudit(ctx context.Context, event AuditEvent) error
}

// LinksRepository читает всё, из чего строится ответ GetCustomerAccessLinks (§5).
//
// Порт из одного метода, а не набор шагов транзакции как у ApplyRepository:
// нормативного порядка блокировок у read-пути нет (он их вовсе не берёт), и
// согласованность нескольких операторов — забота адаптера, а не use case.
type LinksRepository interface {
	// LoadCustomerLinks возвращает один согласованный снимок состояния customer.
	// Отсутствие корневой строки — domain.ErrCustomerNotFound.
	LoadCustomerLinks(ctx context.Context, customerID string) (CustomerLinks, error)
}

// CustomerLinks — снимок, из которого выводятся состояния всех ссылок customer.
//
// Now и ExpiresAt лежат здесь, а не в каждой строке: срок действия применяется ко
// всему customer сразу (§4), и раздать его по строкам значило бы допустить снимок,
// в котором одна ссылка истекла, а другая нет.
type CustomerLinks struct {
	// Now — время PostgreSQL того же снимка, что и остальные поля (решение 2).
	Now       time.Time
	ExpiresAt time.Time

	// Accesses отсортированы по (kind, logical_target_key, access_id) — порядок
	// ответа задаёт §5, и он же порядок строк запроса. Ретайрнутые access и цели,
	// отсутствующие в текущем manifest, сюда не попадают (§5, решение 17).
	Accesses []AccessLinkSource
}

// AccessLinkSource — сырые факты об одном access, из которых выводится ссылка.
//
// Готового состояния здесь нет намеренно: §5 фиксирует, что отдельный
// effective/block state не хранится, и выводить его обязан домен, а не SQL.
type AccessLinkSource struct {
	Kind         domain.AccessKind
	DesiredState domain.DesiredState
	ApplyState   domain.ApplyState

	// QuotaExhausted — exhausted_at IS NOT NULL у входной ноды в текущем периоде.
	QuotaExhausted bool

	// Entry — публичные параметры входной ноды. Для FREEDOM это сама цель, для
	// BRIDGE — entry_node_id связи: на EXIT credential customer не ставится (§4).
	Entry domain.NodePublic

	// BridgeDisplayName — display_name связи; у FREEDOM пусто. Какое из двух имён
	// уходит во фрагмент URI, решает §8, поэтому решает Go, а не запрос.
	BridgeDisplayName string

	// Credential — зашифрованный client_uuid. Расшифровывается только для готовой
	// ссылки и только на время сборки ответа (§7, §8).
	Credential crypto.SealedCredential
}

// ManifestRepository открывает транзакцию приёма манифеста (§6).
type ManifestRepository interface {
	// WithinManifestTx выполняет fn в одной транзакции, предварительно
	// сериализовав приём: одновременно применяется не более одного снапшота
	// (решение 28). nil коммитит, ошибка откатывает.
	WithinManifestTx(ctx context.Context, fn func(ManifestTx) error) error
}

// ManifestTx — шаги приёма манифеста.
//
// Порядок записи внутри WritePlan нормативным списком §11.1 не описан: тот
// перечисляет строки состояния customer, а приём манифеста их не трогает вовсе.
// Поэтому шагов здесь три, а не по одному на таблицу: разделять их значило бы
// изобретать порядок, которого спека не требует.
type ManifestTx interface {
	// LoadProjection читает принятое состояние целиком: ≤100 нод и ≤900 связей
	// (§13) читаются одним заходом дешевле, чем выборочно.
	LoadProjection(ctx context.Context) (domain.ManifestProjection, error)

	// WritePlan проецирует снапшот и ставит джобу материализации. На
	// идемпотентном повторе не вызывается вовсе (решение 21).
	WritePlan(ctx context.Context, plan domain.ManifestPlan) error

	// AppendAudit добавляет запись в audit_events (§15).
	AppendAudit(ctx context.Context, event AuditEvent) error
}

// MaterializationRepository открывает транзакцию одного шага воркера
// материализации (§13).
type MaterializationRepository interface {
	WithinMaterializationTx(ctx context.Context, fn func(MaterializationTx) error) error
}

// MaterializationJob — взятая в работу джоба (§6, §13).
type MaterializationJob struct {
	Revision int64
	// Cursor — последний обработанный customer_id; пустая строка означает, что
	// обход ещё не начинался.
	Cursor string
}

// MaterializationTx — шаги одного шага воркера.
//
// Чтения состояния customer перечислены поимённо, а не свёрнуты в один
// LoadCustomerState, по той же причине, что и в ApplyTx: их порядок нормативен
// (§11.1), и спрятав его в SQL-слой, мы сделали бы его непроверяемым без базы.
// Порядок здесь тот же самый — entitlement, период, расход, затем чтения без
// locks.
type MaterializationTx interface {
	// Now — время начала транзакции (решение 2).
	Now(ctx context.Context) (time.Time, error)

	// ClaimJob берёт lease самой старой незавершённой джобы. nil означает, что
	// работы нет. Просроченный чужой lease подбирается как свободный (§13).
	ClaimJob(ctx context.Context, owner string, leaseTTL time.Duration) (*MaterializationJob, error)

	// NextCustomer возвращает первого customer после курсора в порядке
	// customer_id. Пустая строка означает, что обход завершён.
	NextCustomer(ctx context.Context, afterCustomerID string) (string, error)

	// AdvanceCursor фиксирует прогресс в той же транзакции, что и изменения
	// customer (решение 34).
	AdvanceCursor(ctx context.Context, revision int64, customerID string) error

	// CompleteJob переводит джобу в DONE.
	CompleteJob(ctx context.Context, revision int64) error

	LockEntitlement(ctx context.Context, customerID string) (*domain.Entitlement, error)
	LockOpenQuotaPeriod(ctx context.Context, customerID string) (*domain.QuotaPeriod, error)
	LockNodeQuotaUsage(ctx context.Context, periodID uuid.UUID) ([]domain.NodeQuotaUsage, error)
	LoadTopology(ctx context.Context, fleetID int64) (domain.FleetTopology, error)
	LoadAccesses(ctx context.Context, customerID string) ([]domain.Access, error)

	// LoadLiveNodes — ноды, присутствующие в текущем манифесте глобально. По ним
	// ретайр разводится на «доставить удаление» и «не доставлять никуда» (§6).
	LoadLiveNodes(ctx context.Context) ([]domain.NodeID, error)

	// WriteMaterialization записывает план в порядке блокировок §11.1.
	WriteMaterialization(ctx context.Context, plan MaterializedManifestPlan) error
}

// MaterializedManifestPlan — доменный план материализации, дополненный
// идентификаторами, credentials и операциями outbox.
type MaterializedManifestPlan struct {
	CustomerID string
	// PeriodID — текущий открытый период: в него заводятся строки расхода новых
	// нод (§13).
	PeriodID uuid.UUID

	Plan domain.MaterializationPlan

	// NewAccesses соответствует Plan.CreateAccesses один к одному и в том же
	// порядке.
	NewAccesses []NewAccess
	Operations  []AgentOperation
}

// UsageRepository — состояние учёта трафика для pull worker (§12).
//
// Форма как у DispatchRepository и по той же причине: шаг состоит из нескольких
// транзакций с сетевым вызовом между первой и остальными (§11.1). Batch
// обрабатывается не одной транзакцией, а группами.
type UsageRepository interface {
	// ClaimNode берёт lease ноды, которую пора опросить. nil означает, что
	// опрашивать сейчас нечего: либо все заняты, либо ни одна не «созрела».
	ClaimNode(ctx context.Context, owner string, leaseTTL, minInterval time.Duration) (*ClaimedUsageNode, error)

	// ReleaseNode снимает собственный lease. Без него нода простаивала бы до
	// истечения TTL, который с запасом перекрывает весь шаг вместе с RPC.
	ReleaseNode(ctx context.Context, nodeID domain.NodeID, owner string) error

	// ResolveAccounting сопоставляет accounting_id с владельцами по историческому
	// маппингу: ретайрнутые и погашенные access тоже находятся (§12).
	// Отсутствующие в результате accounting_id неизвестны системе.
	ResolveAccounting(ctx context.Context, accountingIDs []string) (map[string]UsageOwner, error)

	// QuarantineItems кладёт items в карантин и отмечает их обработанными, чтобы
	// один плохой item не блокировал batch навсегда (§12, шаг 6).
	QuarantineItems(ctx context.Context, batch UsageBatchRef, reason string, items []domain.UsageItem) error

	// WithinUsageGroupTx выполняет fn в транзакции одной группы
	// (customer_id, node_id, quota_period_id).
	WithinUsageGroupTx(ctx context.Context, fn func(UsageGroupTx) error) error

	// AdvanceCursor подтверждает позицию спула. Вызывается только после durable
	// commit всех групп batch (решение 63).
	AdvanceCursor(ctx context.Context, nodeID domain.NodeID, cursor nodeagent.UsageCursor) error

	// SetNodeNeedsBootstrap запоминает признак, присланный агентом (§10).
	//
	// Живёт здесь, а не в ReconcileRepository, потому что источник у него один —
	// GetNodeState, а его зовёт только pull worker. Reconcile-воркер спит между
	// заходами и узнал бы о bootstrap с задержкой в целый интервал.
	SetNodeNeedsBootstrap(ctx context.Context, nodeID domain.NodeID, needsBootstrap bool) error
}

// ReconcileRepository — состояние authoritative reconcile (§10).
//
// Форма как у DispatchRepository: шаг состоит из двух транзакций с сетевым
// вызовом между ними, потому что §11.1 запрещает держать транзакцию открытой во
// время обращения к агенту.
type ReconcileRepository interface {
	// ClaimNodeForReconcile берёт lease ноды и её полный набор ОДНОЙ
	// транзакцией. nil означает, что reconcile-ить сейчас нечего.
	//
	// Набор возвращается вместе с lease намеренно: §10 требует фиксировать
	// desired_revision и читать users под одной блокировкой строки ноды, иначе
	// зафиксированная ревизия не описывала бы прочитанный набор.
	ClaimNodeForReconcile(
		ctx context.Context,
		owner string,
		leaseTTL, minInterval time.Duration,
	) (*ClaimedReconcileNode, error)

	// ReleaseNodeReconcile снимает собственный lease.
	ReleaseNodeReconcile(ctx context.Context, nodeID domain.NodeID, owner string) error

	// AcceptReconcile применяет результат, если desired_revision ноды не
	// сдвинулась. accepted=false означает, что desired state уехал, пока мы
	// ходили к агенту: набор на проводе устарел и принимать его нельзя (§10).
	AcceptReconcile(ctx context.Context, acceptance ReconcileAcceptance) (accepted bool, err error)
}

// ClaimedReconcileNode — нода, взятая на reconcile, вместе с её набором (§10).
type ClaimedReconcileNode struct {
	NodeID   domain.NodeID
	Endpoint nodeagent.Endpoint

	// Flow — параметр входной ноды из её public_config, общий для всех её юзеров
	// (§8, §9).
	Flow string

	// DesiredRevision зафиксирована в момент чтения набора. Ею же результат
	// принимается: сдвиг означает, что набор устарел.
	DesiredRevision int64

	// NeedsBootstrap — состояние агента новое или повреждено. Разводит два режима
	// шага: такой ноде набор нужен целиком, всем остальным хватает сверки
	// инвентаря (§16). Читается до сброса флага, который делает приём результата.
	NeedsBootstrap bool

	// Users — полный набор фактически разрешённых desired PRESENT (§10). Пустой
	// набор легален и означает, что backend-owned юзеров на ноде нет.
	Users []ReconcileUser
}

// ReconcileUser — один юзер набора до расшифрования.
type ReconcileUser struct {
	AccessID     uuid.UUID
	AccountingID string
	EgressKey    string
	Credential   crypto.SealedCredential
}

// ReconcileAcceptance — что записать по итогу принятого reconcile (§10).
type ReconcileAcceptance struct {
	NodeID          domain.NodeID
	DesiredRevision int64

	// AppliedAccessIDs — те access, что уехали в наборе. Перечислены поимённо, а
	// не выведены запросом заново: между чтением набора и этой транзакцией
	// прошёл сетевой вызов, и повторный вывод мог бы отметить APPLIED то, чего
	// агент не получал.
	AppliedAccessIDs []uuid.UUID
}

// UsageRetentionRepository — ретенция реестра дедупа (§12).
//
// Отдельный порт от UsageRepository, хотя адаптер у них один: у ретенции нет
// ничего общего с опросом ноды, кроме таблицы. Прунеру незачем уметь брать
// lease и разрешать accounting_id, а расширять UsageRepository седьмым методом
// значило бы дать ему всё это.
type UsageRetentionRepository interface {
	// PruneProcessedUsageItems удаляет до limit записей, которые агент заведомо
	// не пришлёт повторно, и возвращает число удалённых.
	//
	// Граница возраста приходит длительностью, а не моментом времени: считает её
	// SQL от now(), потому что часы — это база (решение 2). Так же устроен
	// minInterval у ClaimNode.
	PruneProcessedUsageItems(ctx context.Context, retention time.Duration, limit int) (int, error)
}

// UsageGroupTx — шаги транзакции одной группы в порядке блокировок §11.1:
// entitlement → quota_periods → node_quota_usage → traffic_usage_items_processed
// → vpn_nodes → vpn_accesses → agent_operations.
type UsageGroupTx interface {
	// Now — время начала транзакции (решение 2). Им отмечается exhausted_at.
	Now(ctx context.Context) (time.Time, error)

	// LockEntitlement берёт FOR UPDATE корневой строки customer (§11.1, шаг 1).
	LockEntitlement(ctx context.Context, customerID string) (*domain.Entitlement, error)

	// LockPeriodAt берёт FOR UPDATE период, в который попадает момент сбора batch
	// (шаг 2). nil означает, что подходящего периода нет вовсе.
	LockPeriodAt(ctx context.Context, customerID string, collectedAt time.Time) (*UsagePeriod, error)

	// LockNodeUsage берёт FOR UPDATE строку расхода ноды, заводя её при
	// отсутствии (шаг 3). Именно этот lock сериализует несколько access одного
	// customer на одной ноде, поэтому порог активируется один раз (§12).
	LockNodeUsage(ctx context.Context, periodID uuid.UUID, nodeID domain.NodeID) (domain.NodeQuotaUsage, error)

	// RegisterProcessed идемпотентно регистрирует items и возвращает только НОВЫЕ
	// (шаг 4). Уже зарегистрированные не возвращаются и не начисляются.
	RegisterProcessed(
		ctx context.Context,
		batch UsageBatchRef,
		periodID uuid.UUID,
		result domain.UsageItemResult,
		items []domain.UsageItem,
	) ([]domain.UsageItem, error)

	// LoadNodeAccesses читает ВСЕ нератайрнутые access customer на этой ноде.
	// Row locks не берёт: корневая строка уже заблокирована (§11.1).
	LoadNodeAccesses(ctx context.Context, customerID string, nodeID domain.NodeID) ([]domain.Access, error)

	// WriteUsageGroup записывает начисление, отметку исчерпания и снятие access
	// (шаги 3, 5–7).
	WriteUsageGroup(ctx context.Context, plan MaterializedUsageGroup) error
}

// ClaimedUsageNode — взятая в опрос нода.
type ClaimedUsageNode struct {
	NodeID   domain.NodeID
	Endpoint nodeagent.Endpoint
	// Cursor — подтверждённая позиция; она уходит агенту как
	// acknowledged_usage_through.
	Cursor nodeagent.UsageCursor
}

// UsageBatchRef — координаты batch для реестра идемпотентности и карантина.
type UsageBatchRef struct {
	NodeID   domain.NodeID
	SpoolID  string
	Sequence uint64
}

// UsagePeriod — период вместе с признаком закрытости.
//
// Признак отдельным полем, а не выведенным из closed_at в домене: домену нужен
// только факт, а момент закрытия ни на что не влияет (§11.1).
type UsagePeriod struct {
	Period domain.QuotaPeriod
	Closed bool
}

// UsageOwner — владелец accounting_id по историческому маппингу.
type UsageOwner struct {
	AccessID    uuid.UUID
	CustomerID  string
	EntryNodeID domain.NodeID
}

// MaterializedUsageGroup — доменный план группы, дополненный операциями outbox.
type MaterializedUsageGroup struct {
	CustomerID string
	NodeID     domain.NodeID
	PeriodID   uuid.UUID

	Plan domain.UsageGroupPlan

	// Operations соответствует Plan.DesiredChanges один к одному: каждый
	// погашенный квотой access получает EnsureUserAbsent (§12, шаг 5).
	Operations []AgentOperation
}

// ExpiryRepository открывает транзакцию одного шага expiry worker (§13).
type ExpiryRepository interface {
	WithinExpiryTx(ctx context.Context, fn func(ExpiryTx) error) error
}

// ExpiryTx — шаги одного шага expiry worker в порядке блокировок §11.1.
//
// Шагов меньше, чем у остальных путей: истечение не читает ни топологию, ни
// квоту. Для истёкшего customer desired state равен ABSENT при любом состоянии
// квоты, поэтому шаг 3 нормативного порядка пропускается целиком (решение 56).
type ExpiryTx interface {
	// Now — время начала транзакции (решение 2). Тот же момент, что и now() в
	// выборке due customer: оба берутся из одной транзакции.
	Now(ctx context.Context) (time.Time, error)

	// LockNextDueCustomer берёт FOR UPDATE SKIP LOCKED одного истёкшего customer,
	// у которого ещё остались PRESENT access. nil означает, что работы нет.
	//
	// Джобы и lease у воркера нет: row lock — вся его координация (решение 54).
	LockNextDueCustomer(ctx context.Context) (*ExpiredCustomer, error)

	// LoadAccesses читает ВСЕ access customer. Row locks не берёт: конкурирующий
	// writer невозможен под lock корневой строки (§11.1).
	LoadAccesses(ctx context.Context, customerID string) ([]domain.Access, error)

	// WriteExpiry записывает план в порядке блокировок §11.1: vpn_nodes,
	// vpn_accesses, agent_operations.
	WriteExpiry(ctx context.Context, plan MaterializedExpiryPlan) error

	// AppendAudit добавляет запись в audit_events. §15 требует аудит customer
	// expiry наравне с Apply и renewal.
	AppendAudit(ctx context.Context, event AuditEvent) error
}

// ExpiredCustomer — корневая строка истёкшего customer под locком.
type ExpiredCustomer struct {
	CustomerID  string
	Entitlement domain.Entitlement
}

// MaterializedExpiryPlan — доменный план, дополненный операциями outbox.
type MaterializedExpiryPlan struct {
	CustomerID string
	Plan       domain.ExpiryPlan
	// Operations соответствует Plan.DesiredChanges один к одному и в том же
	// порядке: каждый погашенный access получает EnsureUserAbsent (§9).
	Operations []AgentOperation
}

// DispatchRepository — состояние outbox для диспетчера операций (§9, §11.1).
//
// Форма отличается от остальных репозиториев, потому что шаг диспетчера состоит из
// ДВУХ транзакций с сетевым вызовом между ними: §11.1 запрещает держать транзакцию
// открытой во время обращения к node-agent. Единый WithinTx выразить это не может.
type DispatchRepository interface {
	// ReapExpiredLeases возвращает в оборот операции умерших воркеров и сообщает,
	// сколько собрал. Устаревшая desired_version переводится в SUPERSEDED,
	// актуальная — обратно в очередь (§9, решение 49).
	ReapExpiredLeases(ctx context.Context, maxReaped int32) (int64, error)

	// LeaseNext берёт lease готовой операции и собирает payload из актуальных строк
	// access и ноды. nil означает, что отправлять нечего.
	//
	// Метод один, а не набор шагов транзакции как у ApplyTx: выбора порядка тут нет
	// — это один оператор, трогающий одну таблицу.
	LeaseNext(ctx context.Context, owner string, leaseTTL time.Duration) (*LeasedOperation, error)

	// WithinResultTx выполняет fn в транзакции записи результата. Шагов два, и их
	// порядок нормативен (§11.1): сначала vpn_accesses, потом agent_operations.
	WithinResultTx(ctx context.Context, fn func(ResultTx) error) error
}

// ResultTx — шаги транзакции записи исхода доставки в порядке блокировок §11.1.
type ResultTx interface {
	// Now — время начала транзакции. Первый оператор, как и в остальных путях:
	// next_attempt_at сравнивается с now() базы, и считать его от часов процесса
	// значило бы разъехаться с тем, кто его читает (решение 2).
	Now(ctx context.Context) (time.Time, error)

	// SetAccessApplyState проецирует исход на строку access и возвращает false,
	// если desired_version уже ушла вперёд: тогда строка не тронута, и результат
	// устаревшей операции не переписал состояние актуальной (§11.1).
	SetAccessApplyState(
		ctx context.Context,
		accessID uuid.UUID,
		desiredVersion int64,
		state domain.ApplyState,
	) (fresh bool, err error)

	// CompleteOperation записывает статус самой операции и снимает lease.
	CompleteOperation(ctx context.Context, result OperationResult) error
}

// LeasedOperation — взятая в работу операция вместе с payload (§9).
//
// Payload не хранится в agent_operations и собран здесь из актуальных строк
// access и ноды непосредственно перед RPC — это прямое требование §9.
type LeasedOperation struct {
	OperationID    uuid.UUID
	AccessID       uuid.UUID
	DesiredVersion int64
	// AttemptCount уже увеличен взятием lease (решение 48).
	AttemptCount int32
	// DesiredState выведено адаптером из operation_type: словарь значений колонки
	// остаётся внутри postgres, как и при записи (§9).
	DesiredState domain.DesiredState

	// AccessDesiredVersion — версия строки access на момент взятия lease. Больше
	// DesiredVersion означает, что звонить агенту уже незачем (§11.1).
	AccessDesiredVersion int64

	Endpoint     nodeagent.Endpoint
	AccountingID string
	EgressKey    string
	// Flow — параметр входной ноды из её public_config (§8, §9).
	Flow string

	// Credential заполнен только для PRESENT: удаление матчится по accounting_id, и
	// расшифровывать ради него секрет незачем (§7, §9).
	Credential crypto.SealedCredential
}

// OperationResult — что записать в строку операции по итогу попытки.
type OperationResult struct {
	OperationID uuid.UUID
	Status      domain.OperationStatus
	// NextAttemptAt непусто только у RETRY_WAIT.
	NextAttemptAt *time.Time
	Completed     bool

	// ErrorCode — стабильный код исхода для наблюдаемости (§15). Заполняется и на
	// успехе: по нему видно, применил агент изменение или подтвердил уже точное
	// состояние.
	ErrorCode string
	// ErrorMessage — санитизированная диагностика агента. Секретов не содержит.
	ErrorMessage string
}

// MaterializedPlan — доменный план, дополненный тем, что домен принципиально не
// производит: идентификаторами, accounting ID и зашифрованными credentials.
//
// Домен возвращает diff и остаётся детерминированным; недетерминированную часть
// добавляет use case из портов IDs и CredentialSealer.
type MaterializedPlan struct {
	CustomerID string
	// FleetID нужен только при создании корневой строки: смена fleet существующего
	// customer запрещена и отсекается доменом (§5, правило 5).
	FleetID int64
	// CommandNumber попадает в last_command_number при успешном commit.
	CommandNumber uint64

	Plan domain.ApplyPlan

	// PeriodID — период, в который пишутся изменения квоты: новый при
	// Plan.OpenNewPeriod, иначе уже открытый. uuid.Nil допустим только на плане,
	// который периода не касается.
	PeriodID uuid.UUID

	// NewAccesses соответствует Plan.CreateAccesses один к одному и в том же
	// порядке.
	NewAccesses []NewAccess

	// Operations — записи transactional outbox (§9). Операция выпускается только по
	// фактическому изменению desired-кортежа; новый ABSENT-access операции не
	// получает (решение 3.2).
	Operations []AgentOperation
}

// NewAccess — создаваемый access вместе со своей идентичностью и credential.
type NewAccess struct {
	Spec         domain.NewAccessSpec
	AccessID     uuid.UUID
	AccountingID string
	// Credential — зашифрованный client_uuid; открытое значение в план не попадает
	// и живёт только внутри вызова Seal (§7).
	Credential crypto.SealedCredential
}

// AgentOperation — операция агенту в outbox (§9).
//
// Тип операции здесь не хранится строкой: он однозначно выводится из DesiredState
// (PRESENT → EnsureUserPresent, ABSENT → EnsureUserAbsent), и словарь значений
// колонки operation_type остаётся внутри адаптера postgres.
type AgentOperation struct {
	OperationID    uuid.UUID
	NodeID         domain.NodeID
	AccessID       uuid.UUID
	DesiredState   domain.DesiredState
	DesiredVersion int64
}
