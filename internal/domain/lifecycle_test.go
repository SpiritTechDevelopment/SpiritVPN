package domain

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestClassifyCommand(t *testing.T) {
	ent := &Entitlement{LastCommandNumber: 10, LastCommandFingerprint: []byte("same")}
	tests := []struct {
		name        string
		number      uint64
		fingerprint []byte
		want        CommandOrder
		wantErr     error
	}{
		{"new", 11, []byte("new"), CommandNew, nil},
		{"stale", 9, []byte("old"), CommandStale, nil},
		{"replay", 10, []byte("same"), CommandReplay, nil},
		{"conflict", 10, []byte("other"), 0, ErrCommandNumberConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ClassifyCommand(tt.number, tt.fingerprint, ent)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ошибка %v, ожидалась %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("результат %v, ожидался %v", got, tt.want)
			}
		})
	}
}

func TestPlanForceAbsentBumpsEveryAccessIncludingRetired(t *testing.T) {
	a := uuid.New()
	b := uuid.New()
	c := uuid.New()
	changes := PlanForceAbsent([]Access{
		{ID: a, EntryNodeID: "node-a", DesiredState: DesiredStatePresent, DesiredVersion: 3},
		{ID: b, EntryNodeID: "node-b", DesiredState: DesiredStateAbsent, DesiredVersion: 7},
		{ID: c, EntryNodeID: "node-c", DesiredVersion: 11, Retired: true},
	})
	if len(changes) != 3 {
		t.Fatalf("изменений %d, ожидалось 3", len(changes))
	}
	versions := map[uuid.UUID]int64{}
	for _, change := range changes {
		if change.DesiredState != DesiredStateAbsent {
			t.Fatalf("state %s", change.DesiredState)
		}
		versions[change.AccessID] = change.DesiredVersion
	}
	if versions[a] != 4 || versions[b] != 8 || versions[c] != 12 {
		t.Fatalf("версии %#v", versions)
	}
}

func TestPlanApplyReactivatesDeletedTombstone(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	plan, err := PlanApply(ApplyInput{
		Now:         now,
		Command:     ApplyCommand{CustomerID: "customer", FleetID: 9, UsageQuotaBytes: 100, ExpiresAt: now.Add(time.Hour), CommandNumber: 8},
		Entitlement: &Entitlement{Lifecycle: CustomerLifecycleDeleted, LastCommandNumber: 7, DesiredVersion: 4},
		Topology:    FleetTopology{FleetID: 9, Nodes: []NodeID{"node-a"}},
	})
	if err != nil {
		t.Fatalf("PlanApply: %v", err)
	}
	if !plan.ReactivateEntitlement || plan.CreateEntitlement {
		t.Fatalf("неверный create/reactivate: %+v", plan)
	}
	if !plan.OpenNewPeriod || len(plan.CreateAccesses) != 1 || plan.CreateAccesses[0].DesiredState != DesiredStatePresent {
		t.Fatalf("неполный план реактивации: %+v", plan)
	}
}

func TestBlockedLifecycleNeverProducesPresent(t *testing.T) {
	now := time.Now().UTC()
	if got := DesiredStateForLifecycle(CustomerLifecycleBlocked, now, now.Add(time.Hour), false); got != DesiredStateAbsent {
		t.Fatalf("DesiredStateForLifecycle = %s", got)
	}
}
