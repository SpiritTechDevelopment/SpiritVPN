package domain

import (
	"cmp"
	"slices"
	"time"
)

// ApplyInput — снимок состояния, достаточный для планирования одной команды
// ApplyCustomerAccess.
type ApplyInput struct {
	// Now — время начала транзакции PostgreSQL (now()), а не часы процесса.
	Now time.Time

	Command ApplyCommand

	// Entitlement — корневая строка customer под row lock; nil означает нового
	// customer.
	Entitlement *Entitlement

	// Topology — текущая проекция fleet из manifest.
	Topology FleetTopology

	// Accesses — ВСЕ access customer, включая ретайрнутые: они нужны для расчёта
	// поколения при повторном появлении ранее удалённой цели (§4).
	Accesses []Access

	// OpenPeriod — единственный период с closed_at IS NULL; nil допустим только
	// для нового customer.
	OpenPeriod *QuotaPeriod

	// NodeUsage — расход по нодам внутри OpenPeriod.
	NodeUsage []NodeQuotaUsage
}

// ApplyPlan — полный набор изменений, который транзакция обязана записать. Домен
// ничего не мутирует: он возвращает diff, а слой postgres его применяет.
//
// Такая форма даёт две вещи. Порядок блокировок §11.1 применяется к готовому
// плану — списки уже отсортированы по node_id и access_id, и он не зависит от
// того, в каком порядке домен обходил входные данные. И пустой план является
// формальным определением no-op из правил 4 и 7 §5: last_command_number
// двигается, операции не создаются.
type ApplyPlan struct {
	Decision ApplyDecision

	// CreateEntitlement — вставить корневую строку вместо обновления.
	CreateEntitlement bool
	// ExpiresAt — целевой момент окончания. При QUOTA_CHANGE совпадает с уже
	// сохранённым.
	ExpiresAt time.Time
	// EntitlementDesiredVersion — новое значение customer_entitlements.desired_version;
	// растёт только на непустом плане (решение 3.5).
	EntitlementDesiredVersion int64

	// OpenNewPeriod — закрыть текущий период (если он есть) и открыть новый тем
	// же timestamp; расход начинается с нуля (§5, §11).
	OpenNewPeriod bool
	// UpdatePeriodQuota — сменить лимит открытого периода, не трогая накопленный
	// расход (§5, правило 7).
	UpdatePeriodQuota bool
	// QuotaBytes — лимит на каждую ноду отдельно.
	QuotaBytes uint64
	// NodeQuotaInits — ноды, которым нужна строка расхода с нулевыми counters и
	// пустым exhausted_at: все ноды fleet нового периода (§5). Внутри уже
	// открытого периода Apply строк не заводит — ноде, добавленной в fleet позже,
	// строку создаёт materialization job вместе с её FREEDOM access (§13).
	// Отсортированы по node_id.
	NodeQuotaInits []NodeID
	// NodeQuotaChanges — постановка и снятие exhausted_at под новый лимит внутри
	// того же периода. Отсортированы по node_id.
	NodeQuotaChanges []NodeQuotaChange

	// CreateAccesses — недостающие под текущую топологию access. Идентификаторы и
	// credentials выдаёт слой приложения.
	CreateAccesses []NewAccessSpec
	// DesiredChanges — существующие access, у которых меняется desired state.
	// Отсортированы по access_id.
	DesiredChanges []DesiredChange

	// TouchedNodes — ноды, у которых меняется состав desired-юзеров. Их строки
	// блокируются в этом порядке, и каждой ровно один раз увеличивается
	// desired_revision, независимо от числа затронутых access (§11.1).
	TouchedNodes []NodeID
}

// IsNoOp сообщает, что принятая команда не меняет целевого состояния. Такая
// команда всё равно обновляет last_command_number, но не создаёт ни операций, ни
// нового периода, ни повторного сброса трафика (§5, правило 4).
func (p ApplyPlan) IsNoOp() bool {
	return !p.CreateEntitlement &&
		!p.OpenNewPeriod &&
		!p.UpdatePeriodQuota &&
		len(p.NodeQuotaInits) == 0 &&
		len(p.NodeQuotaChanges) == 0 &&
		len(p.CreateAccesses) == 0 &&
		len(p.DesiredChanges) == 0
}

// PlanApply строит план одной команды ApplyCustomerAccess (§5, правила 1–9).
//
// Предусловия, обеспечиваемые вызывающим слоем в этом порядке:
//
//  1. ValidateApplyCommand — до любого обращения к БД;
//  2. row lock корневой строки customer и получение Now из now();
//  3. IsStaleCommand — устаревшая команда завершается OK без side effects и сюда
//     не доходит;
//  4. поиск fleet — только после шага 3, иначе повтор старой команды для
//     исчезнувшего fleet вернул бы NOT_FOUND вместо OK.
func PlanApply(in ApplyInput) (ApplyPlan, error) {
	decision, err := ClassifyApply(in.Now, in.Command, in.Entitlement)
	if err != nil {
		return ApplyPlan{}, err
	}

	plan := ApplyPlan{
		Decision:          decision,
		CreateEntitlement: decision == ApplyDecisionCreate,
		ExpiresAt:         in.Command.ExpiresAt,
		OpenNewPeriod:     decision == ApplyDecisionCreate || decision == ApplyDecisionRenewal,
		QuotaBytes:        in.Command.UsageQuotaBytes,
	}

	// Шаг 1. Состояние квоты, каким оно станет ПОСЛЕ этой транзакции. Именно оно,
	// а не сохранённое, определяет desired state: понижение и повышение лимита
	// должны отражаться немедленно (§5).
	exhausted := make(map[NodeID]bool)

	if plan.OpenNewPeriod {
		// Renewal атомарно даёт нулевой расход и пустой exhausted_at на каждой
		// ноде fleet, поэтому исчерпанных нод после него нет по построению (§5).
		plan.NodeQuotaInits = sortedNodes(in.Topology.Nodes)
	} else {
		if in.OpenPeriod == nil {
			return ApplyPlan{}, ErrOpenPeriodMissing
		}

		plan.UpdatePeriodQuota = in.OpenPeriod.UsageQuotaBytes != in.Command.UsageQuotaBytes
		plan.NodeQuotaChanges = RecomputeExhausted(in.NodeUsage, in.Command.UsageQuotaBytes, in.Now)

		for _, usage := range in.NodeUsage {
			exhausted[usage.NodeID] = IsQuotaExhausted(usage.TotalBytes, in.Command.UsageQuotaBytes)
		}
	}

	// Шаг 2. Материализация набора access под текущую топологию. Из плана
	// используется только Create: рассогласованные с manifest access (Retire,
	// Repoint) Apply не трогает — их приводит в порядок materialization job (§6).
	setPlan := PlanAccessSet(in.Topology, in.Accesses)

	for _, spec := range setPlan.Create {
		spec.DesiredState = DesiredStateFor(in.Now, plan.ExpiresAt, exhausted[spec.EntryNodeID])
		spec.DesiredVersion = 1
		plan.CreateAccesses = append(plan.CreateAccesses, spec)
	}

	// Шаг 3. Пересчёт desired state уже существующих согласованных access.
	plan.DesiredChanges = PlanDesiredChanges(setPlan.InSync, in.Now, plan.ExpiresAt, exhausted)

	// Шаг 4. Ноды, состав юзеров которых меняется.
	plan.TouchedNodes = touchedNodes(plan.CreateAccesses, plan.DesiredChanges)

	// Шаг 5. Версия desired state customer растёт только на непустом плане.
	plan.EntitlementDesiredVersion = nextEntitlementVersion(in.Entitlement, plan)

	return plan, nil
}

// nextEntitlementVersion — счётчик фактических изменений desired state customer
// (решение 3.5). Спекой он не используется; смысл в том, чтобы колонка не
// оставалась вечным нулём.
func nextEntitlementVersion(ent *Entitlement, plan ApplyPlan) int64 {
	if ent == nil {
		return 1
	}
	if plan.IsNoOp() {
		return ent.DesiredVersion
	}
	return ent.DesiredVersion + 1
}

// touchedNodes собирает ноды, которым в этой транзакции уедет хотя бы одна
// операция. Новый access, родившийся ABSENT, ноду не затрагивает: операции у него
// нет, состав юзеров Xray не меняется (решение 3.2).
func touchedNodes(creates []NewAccessSpec, changes []DesiredChange) []NodeID {
	seen := make(map[NodeID]struct{})

	for _, spec := range creates {
		if spec.DesiredState == DesiredStatePresent {
			seen[spec.EntryNodeID] = struct{}{}
		}
	}
	for _, change := range changes {
		seen[change.EntryNodeID] = struct{}{}
	}

	return sortedNodeSet(seen)
}

// sortedNodes — копия списка нод, отсортированная по node_id (порядок
// блокировок §11.1).
func sortedNodes(nodes []NodeID) []NodeID {
	if len(nodes) == 0 {
		return nil
	}
	sorted := slices.Clone(nodes)
	slices.Sort(sorted)
	return sorted
}

func sortedNodeSet(set map[NodeID]struct{}) []NodeID {
	if len(set) == 0 {
		return nil
	}
	nodes := make([]NodeID, 0, len(set))
	for node := range set {
		nodes = append(nodes, node)
	}
	slices.SortFunc(nodes, func(a, b NodeID) int { return cmp.Compare(a, b) })
	return nodes
}
