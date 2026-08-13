package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// expiredEntitlement — корневая строка, срок которой истёк час назад.
func expiredEntitlement() Entitlement {
	return Entitlement{
		FleetID:        1,
		ExpiresAt:      tNow.Add(-time.Hour),
		DesiredVersion: 5,
	}
}

func expiryAccess(node NodeID, state DesiredState) Access {
	return Access{
		ID:               uuid.New(),
		Kind:             AccessKindFreedom,
		LogicalTargetKey: LogicalTargetKey(node),
		Generation:       1,
		EntryNodeID:      node,
		DesiredState:     state,
		DesiredVersion:   3,
	}
}

// TestPlanExpiryTurnsPresentAccessesAbsent — воркер переводит access в
// ABSENT и создаёт Remove в одной транзакции.
func TestPlanExpiryTurnsPresentAccessesAbsent(t *testing.T) {
	accesses := []Access{
		expiryAccess("node-a", DesiredStatePresent),
		expiryAccess("node-b", DesiredStatePresent),
	}

	plan := PlanExpiry(tNow, expiredEntitlement(), accesses)

	if len(plan.DesiredChanges) != 2 {
		t.Fatalf("изменений %d, ожидалось 2", len(plan.DesiredChanges))
	}
	for _, change := range plan.DesiredChanges {
		if change.DesiredState != DesiredStateAbsent {
			t.Errorf("desired_state %s, ожидался ABSENT", change.DesiredState)
		}
		if change.DesiredVersion != 4 {
			t.Errorf("desired_version %d, ожидалась 4", change.DesiredVersion)
		}
	}
	if plan.EntitlementDesiredVersion != 6 {
		t.Errorf("версия корневой строки %d, ожидалась 6", plan.EntitlementDesiredVersion)
	}
}

// TestPlanExpiryBumpsEachNodeOnce — транзакция увеличивает
// desired_revision ноды ровно один раз независимо от числа затронутых access.
func TestPlanExpiryBumpsEachNodeOnce(t *testing.T) {
	// Один FREEDOM и два BRIDGE с одной и той же входной нодой — так выглядит
	// customer на входной ноде двух связей.
	accesses := []Access{
		expiryAccess("node-a", DesiredStatePresent),
		expiryAccess("node-a", DesiredStatePresent),
		expiryAccess("node-a", DesiredStatePresent),
		expiryAccess("node-b", DesiredStatePresent),
	}

	plan := PlanExpiry(tNow, expiredEntitlement(), accesses)

	if len(plan.DesiredChanges) != 4 {
		t.Fatalf("изменений %d, ожидалось 4", len(plan.DesiredChanges))
	}
	if len(plan.TouchedNodes) != 2 {
		t.Fatalf("затронутых нод %d, ожидалось 2: %v", len(plan.TouchedNodes), plan.TouchedNodes)
	}
	// Порядок node_id — это нормативный порядок блокировок.
	if plan.TouchedNodes[0] != "node-a" || plan.TouchedNodes[1] != "node-b" {
		t.Errorf("ноды не отсортированы: %v", plan.TouchedNodes)
	}
}

// TestPlanExpiryIsIdempotent — повторный проход не создаёт вторых Remove.
// На этом же держится то, что воркер сообщает о простое, а не крутит истёкших
// customer вечно.
func TestPlanExpiryIsIdempotent(t *testing.T) {
	accesses := []Access{
		expiryAccess("node-a", DesiredStateAbsent),
		expiryAccess("node-b", DesiredStateAbsent),
	}

	plan := PlanExpiry(tNow, expiredEntitlement(), accesses)

	if !plan.IsNoOp() {
		t.Fatalf("повторный проход выдал план: %+v", plan)
	}
	if len(plan.TouchedNodes) != 0 {
		t.Errorf("пустой план трогает ноды: %v", plan.TouchedNodes)
	}
	// Счётчик корневой строки на пустом плане не двигается.
	if plan.EntitlementDesiredVersion != 5 {
		t.Errorf("версия %d, ожидалась прежняя 5", plan.EntitlementDesiredVersion)
	}
}

// TestPlanExpirySparesRenewedCustomer — expiry после блокировки корневой
// строки перечитывает expires_at, поэтому уже закоммиченный renewal не отменяется.
func TestPlanExpirySparesRenewedCustomer(t *testing.T) {
	renewed := expiredEntitlement()
	renewed.ExpiresAt = tNow.Add(24 * time.Hour)

	plan := PlanExpiry(tNow, renewed, []Access{
		expiryAccess("node-a", DesiredStatePresent),
	})

	if !plan.IsNoOp() {
		t.Fatalf("expiry снял доступ у продлённого customer: %+v", plan)
	}
}

// TestPlanExpiryAtExactSecond — доступ существует, пока current_time <
// expires_at, поэтому ровно в момент истечения он уже снимается.
func TestPlanExpiryAtExactSecond(t *testing.T) {
	entitlement := expiredEntitlement()
	entitlement.ExpiresAt = tNow

	plan := PlanExpiry(tNow, entitlement, []Access{
		expiryAccess("node-a", DesiredStatePresent),
	})

	if plan.IsNoOp() {
		t.Fatal("в момент expires_at доступ остался: граница должна быть строгой")
	}
}

// TestPlanExpiryIgnoresRetired — ретайрнутый access уже ABSENT, второй раз его
// гасить нечем и незачем.
func TestPlanExpiryIgnoresRetired(t *testing.T) {
	retired := expiryAccess("node-a", DesiredStateAbsent)
	retired.Retired = true

	plan := PlanExpiry(tNow, expiredEntitlement(), []Access{
		retired,
		expiryAccess("node-b", DesiredStatePresent),
	})

	if len(plan.DesiredChanges) != 1 {
		t.Fatalf("изменений %d, ожидалось 1", len(plan.DesiredChanges))
	}
	if plan.DesiredChanges[0].EntryNodeID != "node-b" {
		t.Errorf("погашен не тот access: %s", plan.DesiredChanges[0].EntryNodeID)
	}
}
