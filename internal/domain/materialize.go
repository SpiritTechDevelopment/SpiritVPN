package domain

import (
	"slices"
	"time"

	"github.com/google/uuid"
)

// RetireSpec — access, чья логическая цель исчезла из манифеста.
//
// Строка не удаляется: она помечается retired_at и переводится в ABSENT, а
// повторное появление той же цели создаёт новое поколение.
type RetireSpec struct {
	Access Access
	// DesiredVersion — новая версия, прежняя + 1. Растёт всегда, даже когда
	// операция не выпускается: по ней устаревшие операции этого access
	// становятся SUPERSEDED.
	DesiredVersion int64
	// IssueOperation — входная нода всё ещё присутствует в манифесте, значит
	// удаление надо доставить обычным EnsureUserAbsent.
	//
	// У глобально удалённой ноды доставлять некуда: запрещено слать
	// команды на endpoint, которого больше нет в authority manifest, и требует
	// вместо этого supersede pending-операций. Это та самая развилка, которую
	// PlanAccessSet намеренно не принимает — она зависит от того, жива ли нода, а
	// не от того, пропала ли цель.
	IssueOperation bool
}

// RepointChange — у связи сменился egress_tag при неизменных routing_key и паре
// (entry, exit).
//
// Не destructive и не новое поколение: client_uuid, accounting_id и generation
// сохраняются, меняется только цель выхода. Агент переиздаёт персональное
// правило (RemoveRule старого + AddRule нового) в рамках обычного
// EnsureUserPresent.
type RepointChange struct {
	AccessID     uuid.UUID
	EntryNodeID  NodeID
	NewEgressKey string
	// DesiredState считается на общих основаниях: repoint истёкшего customer или
	// исчерпанной ноды не обязан делать access присутствующим.
	DesiredState   DesiredState
	DesiredVersion int64
}

// MaterializationInput — состояние одного customer, против которого
// материализуется текущая топология его fleet.
//
// Команды здесь нет: целевое состояние задаёт манифест, а не product-сервис.
// Поэтому нет и решений про expiry, квоту и периоды — джоба их не меняет.
type MaterializationInput struct {
	// Now — время PostgreSQL той же транзакции.
	Now time.Time
	// Entitlement не указатель: джоба обходит уже существующих customer.
	Entitlement Entitlement

	Topology FleetTopology
	Accesses []Access

	// OpenPeriod и NodeUsage нужны, чтобы новый access на уже исчерпанной ноде
	// родился ABSENT, а не оживил доступ в обход квоты.
	OpenPeriod *QuotaPeriod
	NodeUsage  []NodeQuotaUsage

	// LiveNodes — ноды, присутствующие в текущем манифесте ГЛОБАЛЬНО, а не только
	// во fleet этого customer. Именно глобальное отсутствие означает, что runtime
	// ноды больше не используется.
	LiveNodes []NodeID
}

// MaterializationPlan — расхождение между набором access customer и текущей
// топологией, разложенное под запись.
type MaterializationPlan struct {
	// CreateAccesses — цели, появившиеся в манифесте.
	CreateAccesses []NewAccessSpec
	// DesiredChanges — пересчёт desired state согласованных access. Обычно пуст:
	// джоба не меняет ни expiry, ни квоту. Непустым он становится, когда прошлый
	// проход не довёл состояние до конца.
	DesiredChanges []DesiredChange
	Repoints       []RepointChange
	Retire         []RetireSpec

	// NodeQuotaInits — ноды fleet, у которых в текущем периоде ещё нет строки
	// расхода: новая нода получает нулевой node_quota_usage. Дополняет
	// правило, по которому Apply заводит эти строки только при открытии периода.
	NodeQuotaInits []NodeID

	// TouchedNodes — ноды, состав desired-юзеров которых меняется. Их строки
	// блокируются в этом порядке и получают ровно один инкремент
	// desired_revision.
	TouchedNodes []NodeID

	EntitlementDesiredVersion int64
}

// IsNoOp сообщает, что топология и набор access уже согласованы. Повторная
// материализация одной revision обязана быть идемпотентной, и именно это
// свойство делает её таковой.
func (p MaterializationPlan) IsNoOp() bool {
	return len(p.CreateAccesses) == 0 &&
		len(p.DesiredChanges) == 0 &&
		len(p.Repoints) == 0 &&
		len(p.Retire) == 0 &&
		len(p.NodeQuotaInits) == 0
}

// PlanMaterialize строит план материализации манифеста для одного customer.
//
// Отличия от PlanApply, задающие всю структуру функции:
//
//   - используются ВСЕ четыре списка PlanAccessSet, а не только Create. Retire и
//     Repoint принадлежат именно этому срезу — Apply их вычисляет, но не
//     применяет;
//   - expiry и квота не меняются, а только читаются: они определяют desired state
//     создаваемых и существующих access, но джоба их не пересчитывает;
//   - истёкший customer не получает новых access, но ретайр его касается наравне
//     со всеми: исчезнувшая цель обязана быть ретайрнута независимо от срока
//     (ограничение «только неистёкшим» касается лишь создания).
func PlanMaterialize(in MaterializationInput) (MaterializationPlan, error) {
	if in.OpenPeriod == nil {
		return MaterializationPlan{}, ErrOpenPeriodMissing
	}

	expired := !in.Now.Before(in.Entitlement.ExpiresAt)

	exhausted := make(map[NodeID]bool, len(in.NodeUsage))
	for _, usage := range in.NodeUsage {
		exhausted[usage.NodeID] = IsQuotaExhausted(usage.TotalBytes, in.OpenPeriod.UsageQuotaBytes)
	}

	setPlan := PlanAccessSet(in.Topology, in.Accesses)
	live := nodeSet(in.LiveNodes)

	plan := MaterializationPlan{
		NodeQuotaInits: missingUsageNodes(in, expired),
	}

	// Новые цели. Истёкшему customer access не создаются вовсе: он их всё равно
	// не получит, а создание оставило бы за собой credentials, которые никогда не
	// будут доставлены.
	if !expired {
		for _, spec := range setPlan.Create {
			spec.DesiredState = DesiredStateFor(in.Now, in.Entitlement.ExpiresAt, exhausted[spec.EntryNodeID])
			spec.DesiredVersion = 1
			plan.CreateAccesses = append(plan.CreateAccesses, spec)
		}
	}

	plan.DesiredChanges = PlanDesiredChanges(setPlan.InSync, in.Now, in.Entitlement.ExpiresAt, exhausted)
	plan.Repoints = planRepoints(setPlan.Repoint, in.Now, in.Entitlement.ExpiresAt, exhausted)
	plan.Retire = planRetire(setPlan.Retire, live)

	plan.TouchedNodes = materializedTouchedNodes(plan)
	plan.EntitlementDesiredVersion = in.Entitlement.DesiredVersion
	if !plan.IsNoOp() {
		plan.EntitlementDesiredVersion++
	}

	return plan, nil
}

// missingUsageNodes — ноды fleet без строки расхода в текущем периоде.
//
// Истёкшему customer строки не заводятся: правило касается только неистёкших,
// а renewal всё равно откроет новый период и заведёт строки всем
// нодам fleet заново.
func missingUsageNodes(in MaterializationInput, expired bool) []NodeID {
	if expired {
		return nil
	}

	known := make(map[NodeID]struct{}, len(in.NodeUsage))
	for _, usage := range in.NodeUsage {
		known[usage.NodeID] = struct{}{}
	}

	missing := make([]NodeID, 0)
	for _, node := range in.Topology.Nodes {
		if _, ok := known[node]; !ok {
			missing = append(missing, node)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	slices.Sort(missing)
	return missing
}

// planRepoints пересчитывает egress_key и desired state смещённых связей.
//
// Версия растёт всегда, даже если desired state не изменился: egress_key входит
// в то, что доставляется агенту, значит меняется сам desired-кортеж.
func planRepoints(
	specs []RepointSpec,
	now, expiresAt time.Time,
	exhausted map[NodeID]bool,
) []RepointChange {
	if len(specs) == 0 {
		return nil
	}

	changes := make([]RepointChange, 0, len(specs))
	for _, spec := range specs {
		changes = append(changes, RepointChange{
			AccessID:       spec.Access.ID,
			EntryNodeID:    spec.Access.EntryNodeID,
			NewEgressKey:   spec.NewEgressKey,
			DesiredState:   DesiredStateFor(now, expiresAt, exhausted[spec.Access.EntryNodeID]),
			DesiredVersion: spec.Access.DesiredVersion + 1,
		})
	}

	slices.SortFunc(changes, func(a, b RepointChange) int {
		return compareUUID(a.AccessID, b.AccessID)
	})
	return changes
}

// planRetire раскладывает исчезнувшие цели, разводя их по признаку жизни входной
// ноды.
func planRetire(accesses []Access, live map[NodeID]struct{}) []RetireSpec {
	if len(accesses) == 0 {
		return nil
	}

	specs := make([]RetireSpec, 0, len(accesses))
	for _, access := range accesses {
		_, alive := live[access.EntryNodeID]
		specs = append(specs, RetireSpec{
			Access:         access,
			DesiredVersion: access.DesiredVersion + 1,
			// Уже отсутствующему access доставлять нечего: операция была бы
			// no-op, а её недоставленность портила бы метрику.
			IssueOperation: alive && access.DesiredState == DesiredStatePresent,
		})
	}

	slices.SortFunc(specs, func(a, b RetireSpec) int {
		return compareUUID(a.Access.ID, b.Access.ID)
	})
	return specs
}

// materializedTouchedNodes собирает ноды, состав desired-юзеров которых меняется.
//
// Не затрагивают ноду: новый access, родившийся ABSENT (юзера на ноде не было и
// не будет) и ретайр без операции — у глобально удалённой ноды
// увеличивать desired_revision бессмысленно, доставлять на неё нечего.
func materializedTouchedNodes(plan MaterializationPlan) []NodeID {
	seen := make(map[NodeID]struct{})

	for _, spec := range plan.CreateAccesses {
		if spec.DesiredState == DesiredStatePresent {
			seen[spec.EntryNodeID] = struct{}{}
		}
	}
	for _, change := range plan.DesiredChanges {
		seen[change.EntryNodeID] = struct{}{}
	}
	for _, change := range plan.Repoints {
		seen[change.EntryNodeID] = struct{}{}
	}
	for _, spec := range plan.Retire {
		if spec.IssueOperation {
			seen[spec.Access.EntryNodeID] = struct{}{}
		}
	}

	return sortedNodeSet(seen)
}

func nodeSet(nodes []NodeID) map[NodeID]struct{} {
	set := make(map[NodeID]struct{}, len(nodes))
	for _, node := range nodes {
		set[node] = struct{}{}
	}
	return set
}
