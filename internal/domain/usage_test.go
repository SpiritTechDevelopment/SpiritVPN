package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func usageItem(customerID string, node NodeID, up, down uint64) UsageItem {
	return UsageItem{
		AccountingID:  string(node) + "-" + customerID,
		CustomerID:    customerID,
		AccessID:      uuid.New(),
		EntryNodeID:   node,
		UplinkBytes:   up,
		DownlinkBytes: down,
	}
}

func quotaAccess(node NodeID, state DesiredState) Access {
	return Access{
		ID:               uuid.New(),
		Kind:             AccessKindFreedom,
		LogicalTargetKey: LogicalTargetKey(node),
		Generation:       1,
		EntryNodeID:      node,
		DesiredState:     state,
		DesiredVersion:   2,
	}
}

// TestPlanUsageGroupAccrues — дельты новых items складываются.
func TestPlanUsageGroupAccrues(t *testing.T) {
	plan := PlanUsageGroup(UsageGroupInput{
		Now: tNow,
		Items: []UsageItem{
			usageItem("c1", "node-a", 100, 200),
			usageItem("c1", "node-a", 30, 70),
		},
		QuotaBytes:     1 << 30,
		NodeTotalBytes: 0,
	})

	if plan.Result != UsageItemApplied {
		t.Errorf("исход %s, ожидался APPLIED", plan.Result)
	}
	if plan.Accrual.UplinkBytes != 130 || plan.Accrual.DownlinkBytes != 270 {
		t.Errorf("начислено %d/%d, ожидалось 130/270",
			plan.Accrual.UplinkBytes, plan.Accrual.DownlinkBytes)
	}
	if plan.ExhaustedAt != nil {
		t.Error("порог сработал далеко до лимита")
	}
}

// TestPlanUsageGroupClosedPeriodChangesNothing — закрытый период не меняет
// ни counters, ни exhausted_at, ни access; items только регистрируются.
func TestPlanUsageGroupClosedPeriodChangesNothing(t *testing.T) {
	plan := PlanUsageGroup(UsageGroupInput{
		Now:            tNow,
		Items:          []UsageItem{usageItem("c1", "node-a", 1<<40, 1<<40)},
		PeriodClosed:   true,
		QuotaBytes:     1,
		NodeTotalBytes: 0,
		NodeAccesses:   []Access{quotaAccess("node-a", DesiredStatePresent)},
	})

	if plan.Result != UsageItemIgnoredClosedPeriod {
		t.Errorf("исход %s, ожидался IGNORED_CLOSED_PERIOD", plan.Result)
	}
	if !plan.IsNoOp() {
		t.Errorf("закрытый период что-то изменил: %+v", plan)
	}
}

// TestPlanUsageGroupCrossesThresholdAtEquality — доступ существует, пока
// расход строго меньше лимита, поэтому равенство уже исчерпание.
func TestPlanUsageGroupCrossesThresholdAtEquality(t *testing.T) {
	plan := PlanUsageGroup(UsageGroupInput{
		Now:            tNow,
		Items:          []UsageItem{usageItem("c1", "node-a", 400, 600)},
		QuotaBytes:     1000,
		NodeTotalBytes: 0,
		NodeAccesses: []Access{
			quotaAccess("node-a", DesiredStatePresent),
			quotaAccess("node-a", DesiredStatePresent),
		},
	})

	if plan.ExhaustedAt == nil {
		t.Fatal("расход, равный лимиту, не признан исчерпанием")
	}
	if !plan.ExhaustedAt.Equal(tNow) {
		t.Errorf("отметка %v, ожидалась %v", plan.ExhaustedAt, tNow)
	}
	// Гасятся Все access customer на этой ноде.
	if len(plan.DesiredChanges) != 2 {
		t.Fatalf("погашено access %d, ожидалось 2", len(plan.DesiredChanges))
	}
	for _, change := range plan.DesiredChanges {
		if change.DesiredState != DesiredStateAbsent {
			t.Errorf("desired_state %s, ожидался ABSENT", change.DesiredState)
		}
		if change.DesiredVersion != 3 {
			t.Errorf("desired_version %d, ожидалась 3", change.DesiredVersion)
		}
	}
}

// TestPlanUsageGroupThresholdFiresOnce — порог активируется один раз. Иначе
// каждая следующая пачка заново гасила бы уже погашенные access и плодила Remove.
func TestPlanUsageGroupThresholdFiresOnce(t *testing.T) {
	exhausted := tNow.Add(-time.Hour)

	plan := PlanUsageGroup(UsageGroupInput{
		Now:             tNow,
		Items:           []UsageItem{usageItem("c1", "node-a", 500, 500)},
		QuotaBytes:      1000,
		NodeTotalBytes:  5000,
		NodeExhaustedAt: &exhausted,
		NodeAccesses:    []Access{quotaAccess("node-a", DesiredStateAbsent)},
	})

	// Начисление продолжается: items учитываются и после блокировки.
	if plan.Accrual.UplinkBytes != 500 {
		t.Errorf("начисление прекратилось после исчерпания: %+v", plan.Accrual)
	}
	if plan.ExhaustedAt != nil {
		t.Error("отметка исчерпания переставлена повторно")
	}
	if len(plan.DesiredChanges) != 0 {
		t.Errorf("повторное гашение: %+v", plan.DesiredChanges)
	}
}

// TestPlanUsageGroupIgnoresExpiredAccesses — access истёкшего customer уже ABSENT,
// и квота второй раз их не гасит: expiry worker отработал раньше.
func TestPlanUsageGroupIgnoresExpiredAccesses(t *testing.T) {
	plan := PlanUsageGroup(UsageGroupInput{
		Now:            tNow,
		Items:          []UsageItem{usageItem("c1", "node-a", 1000, 0)},
		QuotaBytes:     100,
		NodeTotalBytes: 0,
		NodeAccesses:   []Access{quotaAccess("node-a", DesiredStateAbsent)},
	})

	if plan.ExhaustedAt == nil {
		t.Fatal("отметка исчерпания не поставлена")
	}
	if len(plan.DesiredChanges) != 0 {
		t.Errorf("уже погашенные access погашены повторно: %+v", plan.DesiredChanges)
	}
}

// TestPlanUsageGroupQuotaIsPerNode — превышение на одной ноде не влияет на
// access того же customer на других.
func TestPlanUsageGroupQuotaIsPerNode(t *testing.T) {
	plan := PlanUsageGroup(UsageGroupInput{
		Now:            tNow,
		Items:          []UsageItem{usageItem("c1", "node-a", 1000, 0)},
		QuotaBytes:     100,
		NodeTotalBytes: 0,
		// В группу попадают только access ноды node-a: запрос их и отбирает.
		NodeAccesses: []Access{quotaAccess("node-a", DesiredStatePresent)},
	})

	if len(plan.DesiredChanges) != 1 {
		t.Fatalf("погашено %d access, ожидался 1", len(plan.DesiredChanges))
	}
	if plan.DesiredChanges[0].EntryNodeID != "node-a" {
		t.Errorf("погашен access ноды %s", plan.DesiredChanges[0].EntryNodeID)
	}
}

// TestGroupUsageItemsSplitsByCustomerAndNode — batch обрабатывается группами
// (customer_id, node_id), а не одной транзакцией.
func TestGroupUsageItemsSplitsByCustomerAndNode(t *testing.T) {
	groups := GroupUsageItems([]UsageItem{
		usageItem("c2", "node-b", 1, 1),
		usageItem("c1", "node-b", 1, 1),
		usageItem("c1", "node-a", 1, 1),
		usageItem("c1", "node-a", 2, 2),
	})

	if len(groups) != 3 {
		t.Fatalf("групп %d, ожидалось 3: %+v", len(groups), groups)
	}

	// Порядок детерминирован, поэтому две реплики берут locks одинаково.
	want := []UsageGroupKey{
		{CustomerID: "c1", NodeID: "node-a"},
		{CustomerID: "c1", NodeID: "node-b"},
		{CustomerID: "c2", NodeID: "node-b"},
	}
	for i, key := range want {
		if groups[i].Key != key {
			t.Errorf("группа %d — %+v, ожидалась %+v", i, groups[i].Key, key)
		}
	}
	if len(groups[0].Items) != 2 {
		t.Errorf("в группе c1/node-a %d items, ожидалось 2", len(groups[0].Items))
	}
}

// TestGroupUsageItemsEmpty — пустой batch не даёт групп и не открывает транзакций.
func TestGroupUsageItemsEmpty(t *testing.T) {
	if groups := GroupUsageItems(nil); len(groups) != 0 {
		t.Errorf("групп %d, ожидалось 0", len(groups))
	}
}
