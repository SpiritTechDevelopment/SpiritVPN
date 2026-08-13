package domain

import (
	"cmp"
	"slices"
	"time"

	"github.com/google/uuid"
)

// UsageItemResult — исход обработки одного usage-item.
//
// Значения совпадают с колонкой traffic_usage_items_processed.result: item
// регистрируется как обработанный при любом исходе, иначе плохой item блокировал
// бы batch навсегда.
type UsageItemResult string

const (
	UsageItemApplied             UsageItemResult = "APPLIED"
	UsageItemIgnoredClosedPeriod UsageItemResult = "IGNORED_CLOSED_PERIOD"
	UsageItemQuarantined         UsageItemResult = "QUARANTINED"
)

// Причины карантина. Стабильны: по ним строятся метрики.
const (
	QuarantineUnknownAccountingID = "UNKNOWN_ACCOUNTING_ID"
	QuarantineNoQuotaPeriod       = "NO_QUOTA_PERIOD"
)

// UsageItem — дельта трафика одного access, уже сопоставленная с владельцем.
//
// Сопоставление делает слой приложения по historical accounting mapping: колонка
// accounting_id уникальна, значения не переиспользуются, а строки access не
// удаляются, поэтому items учитываются и после expiry/retire.
// Нераспознанный accounting_id сюда не доходит — он уходит в карантин раньше.
type UsageItem struct {
	AccountingID string
	CustomerID   string
	AccessID     uuid.UUID
	// EntryNodeID берётся из строки access, а не из того, какую ноду мы опросили:
	// они обязаны совпадать, и расхождение ловится вызывающим.
	EntryNodeID   NodeID
	UplinkBytes   uint64
	DownlinkBytes uint64
}

// Bytes — суммарная дельта item.
func (i UsageItem) Bytes() uint64 { return i.UplinkBytes + i.DownlinkBytes }

// UsageAccrual — начисление на одну строку node_quota_usage.
type UsageAccrual struct {
	UplinkBytes   uint64
	DownlinkBytes uint64
	// Items — какие именно items вошли в начисление. Каждый регистрируется в
	// реестре идемпотентности вместе с counters, в той же транзакции.
	Items []UsageItem
}

// UsageGroupPlan — что сделать с одной группой (customer_id, node_id,
// quota_period_id) в одной короткой транзакции.
type UsageGroupPlan struct {
	// Accrual непусто только для открытого периода.
	Accrual UsageAccrual

	// Result — с каким исходом регистрируются items группы.
	Result UsageItemResult

	// ExhaustedAt непусто, когда именно эта транзакция впервые пересекает порог.
	// Тогда же гасятся access и создаются Remove operations.
	ExhaustedAt *time.Time

	// DesiredChanges — access customer на этой ноде, уходящие в ABSENT.
	// Отсортированы по access_id.
	DesiredChanges []DesiredChange
}

// IsNoOp сообщает, что группа не меняет counters и состояния.
func (p UsageGroupPlan) IsNoOp() bool {
	return len(p.Accrual.Items) == 0 && p.ExhaustedAt == nil && len(p.DesiredChanges) == 0
}

// UsageGroupInput — состояние, под которое считается группа.
type UsageGroupInput struct {
	// Now — время транзакции. Им отмечается exhausted_at.
	Now time.Time

	// Items — новые items группы: уже прошедшие дедуп по
	// (spool_id, sequence, accounting_id). Повторно присланные сюда не доходят.
	Items []UsageItem

	// PeriodClosed отмечает период, закрытый к моменту обработки. Для него
	// counters, exhausted_at и access не меняются, а items регистрируются с
	// IGNORED_CLOSED_PERIOD.
	PeriodClosed bool

	// QuotaBytes — лимит периода на каждую ноду отдельно.
	QuotaBytes uint64
	// NodeTotalBytes — расход ноды в периоде ДО этого начисления.
	NodeTotalBytes uint64
	// NodeExhaustedAt — уже стоящая отметка исчерпания, если она есть.
	NodeExhaustedAt *time.Time

	// NodeAccesses — все нератайрнутые access customer с этой входной нодой.
	// Гасятся все, а не только тот, чей accounting_id приехал.
	NodeAccesses []Access
}

// PlanUsageGroup считает одну группу.
//
// Порог — IsQuotaExhausted, то есть равенство лимиту уже является исчерпанием.
// Отметка ставится ровно один раз: если NodeExhaustedAt уже непуст,
// повторно гасить нечего, а access этой ноды давно ABSENT.
//
// Квота — control-plane threshold наилучшего усилия: дельта, не успевшая в
// спул, уменьшает учтённый total и увеличивает фактическое превышение. Это
// принятый для v1 trade-off, и домен его не компенсирует.
func PlanUsageGroup(in UsageGroupInput) UsageGroupPlan {
	if in.PeriodClosed {
		// Закрытый период не меняет ни counters, ни exhausted_at, ни
		// access. Items всё равно регистрируются — иначе batch не подтвердится.
		return UsageGroupPlan{Result: UsageItemIgnoredClosedPeriod}
	}

	plan := UsageGroupPlan{Result: UsageItemApplied}

	total := in.NodeTotalBytes
	for _, item := range in.Items {
		plan.Accrual.UplinkBytes += item.UplinkBytes
		plan.Accrual.DownlinkBytes += item.DownlinkBytes
		total += item.Bytes()
	}
	plan.Accrual.Items = in.Items

	// Отметка ставится только при первом пересечении: порог активируется один
	// раз, иначе каждая последующая пачка заново гасила бы уже
	// погашенные access и плодила Remove operations.
	if in.NodeExhaustedAt != nil || !IsQuotaExhausted(total, in.QuotaBytes) {
		return plan
	}

	now := in.Now
	plan.ExhaustedAt = &now
	// Срок здесь не проверяется: истёкшего customer уже погасил expiry worker, и
	// его access не PRESENT, поэтому PlanDesiredChanges их не тронет.
	plan.DesiredChanges = PlanDesiredChanges(
		in.NodeAccesses, in.Now, farFuture, map[NodeID]bool{nodeOf(in.NodeAccesses): true})

	return plan
}

// farFuture — заглушка expires_at для пересчёта desired state по квоте.
//
// PlanDesiredChanges принимает срок и квоту вместе, а здесь решает только квота:
// об истечении заботится expiry worker, и подмешивать сюда второе правило значило
// бы дублировать его. Заведомо будущий момент отключает временну́ю половину
// временную часть условия, оставляя работать квотную.
var farFuture = time.Date(9999, time.January, 1, 0, 0, 0, 0, time.UTC)

// nodeOf возвращает входную ноду группы. Все access группы принадлежат одной
// ноде по построению — группировка идёт по (customer_id, node_id, period).
func nodeOf(accesses []Access) NodeID {
	if len(accesses) == 0 {
		return ""
	}
	return accesses[0].EntryNodeID
}

// UsageGroupKey — ключ группировки items batch.
type UsageGroupKey struct {
	CustomerID string
	NodeID     NodeID
}

// GroupUsageItems раскладывает items batch по customer.
//
// Группировка нужна потому, что batch нельзя обрабатывать одной большой
// транзакцией: каждая группа блокирует свой корневой entitlement, и разные
// customer идут независимо. Период в ключ не входит — он один на группу и
// выбирается уже под locком, по collected_at.
//
// Результат отсортирован по (customer_id, node_id): порядок обработки групп
// детерминирован, поэтому две реплики, разбирающие один batch, берут locks в одном
// порядке.
func GroupUsageItems(items []UsageItem) []UsageGroup {
	byKey := make(map[UsageGroupKey][]UsageItem)
	for _, item := range items {
		key := UsageGroupKey{CustomerID: item.CustomerID, NodeID: item.EntryNodeID}
		byKey[key] = append(byKey[key], item)
	}

	groups := make([]UsageGroup, 0, len(byKey))
	for key, grouped := range byKey {
		slices.SortFunc(grouped, func(a, b UsageItem) int {
			return cmp.Compare(a.AccountingID, b.AccountingID)
		})
		groups = append(groups, UsageGroup{Key: key, Items: grouped})
	}

	slices.SortFunc(groups, func(a, b UsageGroup) int {
		return cmp.Or(
			cmp.Compare(a.Key.CustomerID, b.Key.CustomerID),
			cmp.Compare(a.Key.NodeID, b.Key.NodeID),
		)
	})
	return groups
}

// UsageGroup — items одного customer на одной ноде.
type UsageGroup struct {
	Key   UsageGroupKey
	Items []UsageItem
}
