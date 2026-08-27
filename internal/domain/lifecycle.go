package domain

import (
	"bytes"
	"slices"

	"github.com/google/uuid"
)

// CustomerLifecycle — административное desired-состояние customer.
type CustomerLifecycle string

const (
	CustomerLifecycleActive   CustomerLifecycle = "ACTIVE"
	CustomerLifecycleBlocked  CustomerLifecycle = "BLOCKED"
	CustomerLifecycleDeleting CustomerLifecycle = "DELETING"
	CustomerLifecycleDeleted  CustomerLifecycle = "DELETED"
)

// EffectiveLifecycle сохраняет совместимость доменных fixtures, созданных до
// появления lifecycle: нулевое значение означает обычный ACTIVE customer.
func EffectiveLifecycle(ent *Entitlement) CustomerLifecycle {
	if ent == nil || ent.Lifecycle == "" {
		return CustomerLifecycleActive
	}
	return ent.Lifecycle
}

type CommandOrder int

const (
	CommandNew CommandOrder = iota + 1
	CommandStale
	CommandReplay
)

// ClassifyCommand проверяет общий монотонный поток Apply/Block/Delete.
func ClassifyCommand(number uint64, fingerprint []byte, ent *Entitlement) (CommandOrder, error) {
	if ent == nil || number > ent.LastCommandNumber {
		return CommandNew, nil
	}
	if number < ent.LastCommandNumber {
		return CommandStale, nil
	}
	if len(ent.LastCommandFingerprint) == 0 || bytes.Equal(fingerprint, ent.LastCommandFingerprint) {
		return CommandReplay, nil
	}
	return 0, ErrCommandNumberConflict
}

// PlanForceAbsent переводит каждый выбранный access в новую версию ABSENT.
// Даже уже ABSENT access получает новую операцию: административное удаление
// является физической проверкой отсутствия, а не только сменой desired-флага.
func PlanForceAbsent(accesses []Access) []DesiredChange {
	changes := make([]DesiredChange, 0, len(accesses))
	for _, access := range accesses {
		changes = append(changes, DesiredChange{
			AccessID:       access.ID,
			EntryNodeID:    access.EntryNodeID,
			DesiredState:   DesiredStateAbsent,
			DesiredVersion: access.DesiredVersion + 1,
		})
	}
	slices.SortFunc(changes, func(a, b DesiredChange) int {
		return bytes.Compare(a.AccessID[:], b.AccessID[:])
	})
	return changes
}

func TouchedNodesForChanges(changes []DesiredChange) []NodeID {
	seen := make(map[NodeID]struct{}, len(changes))
	for _, change := range changes {
		seen[change.EntryNodeID] = struct{}{}
	}
	return sortedNodeSet(seen)
}

// FindAccess возвращает access по id. Используется чистыми lifecycle-тестами и
// материализацией операций без повторного индексирования в application layer.
func FindAccess(accesses []Access, id uuid.UUID) (Access, bool) {
	for _, access := range accesses {
		if access.ID == id {
			return access, true
		}
	}
	return Access{}, false
}
