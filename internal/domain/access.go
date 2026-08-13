package domain

import (
	"bytes"
	"cmp"
	"slices"

	"github.com/google/uuid"
)

// Target — одна логическая цель текущей топологии fleet, под которую у
// действующего customer должен существовать access.
type Target struct {
	Kind             AccessKind
	LogicalTargetKey LogicalTargetKey
	// EntryNodeID — нода, на которую ставится Xray user. Для FREEDOM это сама
	// нода, для BRIDGE — entry_node_id связи.
	EntryNodeID NodeID
	// EgressKey передаётся агенту дословно: "" (локальный выход) для FREEDOM,
	// egress_tag связи для BRIDGE.
	EgressKey string
}

// TargetsOf разворачивает топологию fleet в набор целей:
//
//	link_count = fleet_node_count + bridge_relation_count
//
// На одной ноде у customer всегда не более одного FREEDOM, но может быть
// несколько BRIDGE — по одному на каждую связь, где эта нода является входной.
// Результат отсортирован по (kind, logical_target_key), чтобы план не
// зависел от порядка строк проекции.
func TargetsOf(topology FleetTopology) []Target {
	targets := make([]Target, 0, len(topology.Nodes)+len(topology.Bridges))

	for _, node := range topology.Nodes {
		targets = append(targets, Target{
			Kind:             AccessKindFreedom,
			LogicalTargetKey: LogicalTargetKey(node),
			EntryNodeID:      node,
			EgressKey:        FreedomEgressKey,
		})
	}

	for _, bridge := range topology.Bridges {
		targets = append(targets, Target{
			Kind:             AccessKindBridge,
			LogicalTargetKey: LogicalTargetKey(bridge.RoutingKey),
			EntryNodeID:      bridge.EntryNodeID,
			EgressKey:        bridge.EgressTag,
		})
	}

	slices.SortFunc(targets, func(a, b Target) int {
		return cmp.Or(
			cmp.Compare(a.Kind, b.Kind),
			cmp.Compare(a.LogicalTargetKey, b.LogicalTargetKey),
		)
	})
	return targets
}

// NewAccessSpec — access, которого не хватает под текущую топологию. Идентичность
// (access_id), accounting_id и client_uuid здесь отсутствуют: их выдаёт слой
// приложения через порты, домен остаётся детерминированным.
type NewAccessSpec struct {
	Kind             AccessKind
	LogicalTargetKey LogicalTargetKey
	// Generation — поколение цели: 1 для первого появления, max+1 при повторном
	// добавлении ранее удалённой цели. Новое поколение получает новые
	// client_uuid и accounting_id.
	Generation  int32
	EntryNodeID NodeID
	EgressKey   string

	// DesiredState вычисляется PlanApply; access может родиться сразу ABSENT,
	// если customer истёк или квота его ноды исчерпана. Операция создаётся только
	// для PRESENT: юзера на ноде ещё не было, и отсутствие — состояние Xray по
	// умолчанию.
	DesiredState DesiredState
	// DesiredVersion нового access всегда 1, независимо от DesiredState.
	DesiredVersion int64
}

// RepointSpec — у существующей связи сменился egress_tag при неизменных
// routing_key и паре (entry, exit). Это не destructive: backend обновляет
// egress_key access и переиздаёт routing rule без смены client_uuid,
// accounting_id и без нового поколения.
type RepointSpec struct {
	Access       Access
	NewEgressKey string
}

// AccessSetPlan — расхождение между текущим набором access customer и целями
// топологии.
//
// Retire и Repoint здесь ВЫЧИСЛЯЮТСЯ, но ApplyCustomerAccess их не применяет: он
// использует их только как признак «access рассогласован с manifest, не трогать».
// Оба действия принадлежат manifest materialization job, где ретайр
// ветвится по тому, жива ли нода: глобально удалённая нода получает
// RETIRED/ABSENT с supersede операций и без единого вызова на endpoint, а
// удаление связи при живой входной ноде — обычный EnsureUserAbsent. Держать эту
// развилку в двух местах нельзя.
type AccessSetPlan struct {
	Create  []NewAccessSpec
	Retire  []Access
	Repoint []RepointSpec
	// InSync — текущие access, согласованные с топологией. Только по ним Apply
	// пересчитывает desired state и выпускает операции.
	InSync []Access
}

// targetKey — ключ уникальности цели внутри customer: unique
// (customer_id, kind, logical_target_key) among not retired.
type targetKey struct {
	kind AccessKind
	key  LogicalTargetKey
}

// PlanAccessSet сопоставляет цели топологии с текущими access customer.
//
// accesses должен содержать ВСЕ access customer, включая ретайрнутые: без них
// нельзя вычислить поколение при повторном появлении ранее удалённой цели.
func PlanAccessSet(topology FleetTopology, accesses []Access) AccessSetPlan {
	// Действующий access на цель ровно один — это гарантирует partial unique
	// index по (customer_id, kind, logical_target_key) WHERE retired_at IS NULL.
	current := make(map[targetKey]Access, len(accesses))
	// Максимальное поколение считается по всей истории цели, включая ретайрнутые.
	maxGeneration := make(map[targetKey]int32, len(accesses))

	for _, access := range accesses {
		key := targetKey{kind: access.Kind, key: access.LogicalTargetKey}
		if access.Generation > maxGeneration[key] {
			maxGeneration[key] = access.Generation
		}
		if !access.Retired {
			current[key] = access
		}
	}

	plan := AccessSetPlan{}
	matched := make(map[targetKey]struct{}, len(current))

	for _, target := range TargetsOf(topology) {
		key := targetKey{kind: target.Kind, key: target.LogicalTargetKey}

		access, exists := current[key]
		if !exists {
			plan.Create = append(plan.Create, NewAccessSpec{
				Kind:             target.Kind,
				LogicalTargetKey: target.LogicalTargetKey,
				Generation:       maxGeneration[key] + 1,
				EntryNodeID:      target.EntryNodeID,
				EgressKey:        target.EgressKey,
			})
			continue
		}

		matched[key] = struct{}{}

		// Единственный источник дрейфа — egress_tag. Входная нода дрейфовать не
		// может: пара (entry_node_id, exit_node_id) неизменяема для существующего
		// routing_key, а у FREEDOM входная нода совпадает с самой целью.
		if access.EgressKey != target.EgressKey {
			plan.Repoint = append(plan.Repoint, RepointSpec{
				Access:       access,
				NewEgressKey: target.EgressKey,
			})
			continue
		}

		plan.InSync = append(plan.InSync, access)
	}

	// Цель пропала из топологии: access рассогласован. Apply его не ретайрит и не
	// выпускает по нему операций — этим занимается materialization job.
	for key, access := range current {
		if _, ok := matched[key]; !ok {
			plan.Retire = append(plan.Retire, access)
		}
	}
	slices.SortFunc(plan.Retire, compareAccessByTarget)

	return plan
}

// compareAccessByTarget упорядочивает access по (kind, logical_target_key,
// access_id) — тому же порядку, в котором access уходят в ответах Customer
// API. Здесь он нужен, чтобы план не зависел от обхода map.
func compareAccessByTarget(a, b Access) int {
	return cmp.Or(
		cmp.Compare(a.Kind, b.Kind),
		cmp.Compare(a.LogicalTargetKey, b.LogicalTargetKey),
		bytes.Compare(a.ID[:], b.ID[:]),
	)
}

// compareUUID упорядочивает по access_id — порядок блокировок vpn_accesses.
// Отдельная функция, потому что сравнение идёт по байтам значения, а не
// по его строковому представлению.
func compareUUID(a, b uuid.UUID) int {
	return bytes.Compare(a[:], b[:])
}
