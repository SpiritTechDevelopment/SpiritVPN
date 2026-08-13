package domain

import (
	"bytes"
	"slices"
	"time"

	"github.com/google/uuid"
)

// DesiredStateFor — эффективность access на его входной ноде:
//
//	current_time < expires_at AND node_quota_period_usage_bytes < usage_quota_bytes
//
// Срок действия применяется ко всему customer, квота — независимо на каждой ноде,
// поэтому исчерпание на одной ноде не влияет на access того же customer на других.
//
// Отдельный effective/block state нигде не хранится: он всегда выводится из
// expires_at и exhausted_at текущего периода.
func DesiredStateFor(now, expiresAt time.Time, nodeExhausted bool) DesiredState {
	if now.Before(expiresAt) && !nodeExhausted {
		return DesiredStatePresent
	}
	return DesiredStateAbsent
}

// DesiredChange — смена desired-кортежа существующего access, требующая новой
// операции агенту.
//
// Тип операции не дублируется отдельным полем: PRESENT даёт EnsureUserPresent,
// ABSENT — EnsureUserAbsent.
type DesiredChange struct {
	AccessID uuid.UUID
	// EntryNodeID нужен, чтобы транзакция заблокировала строку ноды и ровно один
	// раз увеличила её desired_revision.
	EntryNodeID  NodeID
	DesiredState DesiredState
	// DesiredVersion — новая версия, прежняя + 1. Она же попадает в
	// agent_operations.desired_version.
	DesiredVersion int64
}

// PlanDesiredChanges пересчитывает desired state существующих access и оставляет
// только те, у которых он фактически меняется.
//
// accesses — только согласованные с топологией access (AccessSetPlan.InSync):
// рассогласованные Apply не трогает, иначе он либо оживил бы цель, находящуюся в
// процессе удаления, либо запушил бы устаревший egress_key.
//
// nodeExhausted отражает состояние ПОСЛЕ применения изменений квоты этой же
// транзакции, а не сохранённое: понижение и повышение лимита должны немедленно
// отражаться на desired state.
//
// Неизменившийся desired-кортеж не порождает ни новой версии, ни операции —
// на этом держится естественная идемпотентность повторного Apply.
// Результат отсортирован по access_id (нормативный порядок блокировок).
func PlanDesiredChanges(
	accesses []Access,
	now, expiresAt time.Time,
	nodeExhausted map[NodeID]bool,
) []DesiredChange {
	var changes []DesiredChange

	for _, access := range accesses {
		want := DesiredStateFor(now, expiresAt, nodeExhausted[access.EntryNodeID])
		if want == access.DesiredState {
			continue
		}

		changes = append(changes, DesiredChange{
			AccessID:       access.ID,
			EntryNodeID:    access.EntryNodeID,
			DesiredState:   want,
			DesiredVersion: access.DesiredVersion + 1,
		})
	}

	slices.SortFunc(changes, func(a, b DesiredChange) int {
		return bytes.Compare(a.AccessID[:], b.AccessID[:])
	})
	return changes
}
