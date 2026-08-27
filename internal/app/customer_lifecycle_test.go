package app

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
)

type lifecycleRepoFake struct{ tx *lifecycleTxFake }

func (r *lifecycleRepoFake) WithinLifecycleTx(ctx context.Context, fn func(LifecycleTx) error) error {
	return fn(r.tx)
}

type lifecycleTxFake struct {
	now       time.Time
	ent       *domain.Entitlement
	period    *domain.QuotaPeriod
	usage     []domain.NodeQuotaUsage
	accesses  []domain.Access
	topology  domain.FleetTopology
	liveNodes []domain.NodeID
	written   *MaterializedLifecyclePlan
	tombstone bool
	audits    []AuditEvent
}

func (t *lifecycleTxFake) Now(context.Context) (time.Time, error) { return t.now, nil }
func (t *lifecycleTxFake) LockEntitlement(context.Context, string) (*domain.Entitlement, error) {
	return t.ent, nil
}
func (t *lifecycleTxFake) LockOpenQuotaPeriod(context.Context, string) (*domain.QuotaPeriod, error) {
	return t.period, nil
}
func (t *lifecycleTxFake) LockNodeQuotaUsage(context.Context, uuid.UUID) ([]domain.NodeQuotaUsage, error) {
	return t.usage, nil
}
func (t *lifecycleTxFake) LoadAccesses(context.Context, string) ([]domain.Access, error) {
	return t.accesses, nil
}
func (t *lifecycleTxFake) LoadTopology(context.Context, int64) (domain.FleetTopology, error) {
	return t.topology, nil
}
func (t *lifecycleTxFake) LoadLiveNodes(context.Context) ([]domain.NodeID, error) {
	if t.liveNodes == nil {
		return []domain.NodeID{"a", "b", "node"}, nil
	}
	return t.liveNodes, nil
}
func (t *lifecycleTxFake) WriteLifecycle(_ context.Context, plan MaterializedLifecyclePlan) error {
	t.written = &plan
	return nil
}
func (t *lifecycleTxFake) InsertDeletedTombstone(context.Context, string, uint64, []byte) error {
	t.tombstone = true
	return nil
}
func (t *lifecycleTxFake) AppendAudit(_ context.Context, event AuditEvent) error {
	t.audits = append(t.audits, event)
	return nil
}

type lifecycleIDs struct{ operations int }

func (*lifecycleIDs) NewAccessID() (uuid.UUID, error)      { return uuid.New(), nil }
func (*lifecycleIDs) NewQuotaPeriodID() (uuid.UUID, error) { return uuid.New(), nil }
func (i *lifecycleIDs) NewOperationID() (uuid.UUID, error) { i.operations++; return uuid.New(), nil }
func (*lifecycleIDs) NewAccountingID() (string, error)     { return "u.lifecycle-test", nil }
func (*lifecycleIDs) NewClientUUID() (crypto.ClientUUID, error) {
	return crypto.NewClientUUID(uuid.New()), nil
}

func TestAdministrativeBlockForcesAbsentOperations(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	tx := &lifecycleTxFake{
		now: now,
		ent: &domain.Entitlement{Lifecycle: domain.CustomerLifecycleActive, LastCommandNumber: 3, DesiredVersion: 5, ExpiresAt: now.Add(time.Hour)},
		accesses: []domain.Access{
			{ID: uuid.New(), EntryNodeID: "a", DesiredState: domain.DesiredStatePresent, DesiredVersion: 1},
			{ID: uuid.New(), EntryNodeID: "b", DesiredState: domain.DesiredStateAbsent, DesiredVersion: 4},
		},
	}
	ids := &lifecycleIDs{}
	uc := NewSetCustomerAccessState(&lifecycleRepoFake{tx: tx}, ids)
	err := uc.Execute(context.Background(), SetCustomerAccessStateCommand{Command: domain.AdministrativeCommand{
		CustomerID: "customer", Target: domain.CustomerLifecycleBlocked, CommandNumber: 4,
	}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if tx.written == nil || tx.written.Target != domain.CustomerLifecycleBlocked {
		t.Fatalf("план %+v", tx.written)
	}
	if len(tx.written.DesiredChanges) != 2 || len(tx.written.Operations) != 2 || ids.operations != 2 {
		t.Fatalf("changes=%d operations=%d ids=%d", len(tx.written.DesiredChanges), len(tx.written.Operations), ids.operations)
	}
}

func TestAdministrativeBlockSkipsOperationsForNodesOutsideManifest(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	liveAccessID := uuid.New()
	removedAccessID := uuid.New()
	tx := &lifecycleTxFake{
		now:       now,
		ent:       &domain.Entitlement{Lifecycle: domain.CustomerLifecycleActive, DesiredVersion: 5, ExpiresAt: now.Add(time.Hour)},
		liveNodes: []domain.NodeID{"live"},
		accesses: []domain.Access{
			{ID: liveAccessID, EntryNodeID: "live", DesiredState: domain.DesiredStatePresent, DesiredVersion: 1},
			{ID: removedAccessID, EntryNodeID: "removed", DesiredState: domain.DesiredStateAbsent, DesiredVersion: 4, Retired: true},
		},
	}
	ids := &lifecycleIDs{}
	err := NewSetCustomerAccessState(&lifecycleRepoFake{tx: tx}, ids).Execute(
		context.Background(),
		SetCustomerAccessStateCommand{Command: domain.AdministrativeCommand{
			CustomerID: "customer", Target: domain.CustomerLifecycleBlocked, CommandNumber: 1,
		}},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := len(tx.written.DesiredChanges); got != 2 {
		t.Fatalf("desired changes = %d, ожидалось 2", got)
	}
	if got := len(tx.written.Operations); got != 1 || tx.written.Operations[0].AccessID != liveAccessID {
		t.Fatalf("operations = %+v", tx.written.Operations)
	}
	if got := tx.written.AppliedWithoutOperation; len(got) != 1 || got[0] != removedAccessID {
		t.Fatalf("applied without operation = %v", got)
	}
}

func TestAdministrativeBlockWithEmptyManifestIssuesNoOperations(t *testing.T) {
	now := time.Now().UTC()
	tx := &lifecycleTxFake{
		now:       now,
		ent:       &domain.Entitlement{Lifecycle: domain.CustomerLifecycleActive, ExpiresAt: now.Add(time.Hour)},
		liveNodes: []domain.NodeID{},
		accesses:  []domain.Access{{ID: uuid.New(), EntryNodeID: "removed", DesiredState: domain.DesiredStatePresent}},
	}
	err := NewSetCustomerAccessState(&lifecycleRepoFake{tx: tx}, &lifecycleIDs{}).Execute(
		context.Background(),
		SetCustomerAccessStateCommand{Command: domain.AdministrativeCommand{
			CustomerID: "customer", Target: domain.CustomerLifecycleBlocked, CommandNumber: 1,
		}},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(tx.written.Operations) != 0 || len(tx.written.AppliedWithoutOperation) != 1 {
		t.Fatalf("план %+v", tx.written)
	}
}

func TestDeleteUnknownCustomerCreatesCompletedTombstone(t *testing.T) {
	tx := &lifecycleTxFake{now: time.Now().UTC()}
	uc := NewDeleteCustomerAccess(&lifecycleRepoFake{tx: tx}, &lifecycleIDs{}, time.Second)
	state, err := uc.Execute(context.Background(), DeleteCustomerAccessCommand{Command: domain.DeleteCommand{
		CustomerID: "missing", CommandNumber: 10,
	}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if state != domain.CustomerDeletionCompleted || !tx.tombstone {
		t.Fatalf("state=%s tombstone=%v", state, tx.tombstone)
	}
}

func TestDeleteMovesExistingCustomerToDeleting(t *testing.T) {
	now := time.Now().UTC()
	tx := &lifecycleTxFake{
		now:      now,
		ent:      &domain.Entitlement{Lifecycle: domain.CustomerLifecycleBlocked, LastCommandNumber: 4, DesiredVersion: 2},
		accesses: []domain.Access{{ID: uuid.New(), EntryNodeID: "node", DesiredState: domain.DesiredStateAbsent, DesiredVersion: 9}},
	}
	uc := NewDeleteCustomerAccess(&lifecycleRepoFake{tx: tx}, &lifecycleIDs{}, 30*time.Second)
	state, err := uc.Execute(context.Background(), DeleteCustomerAccessCommand{Command: domain.DeleteCommand{
		CustomerID: "customer", CommandNumber: 5,
	}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if state != domain.CustomerDeletionPending || tx.written == nil {
		t.Fatalf("state=%s plan=%+v", state, tx.written)
	}
	if tx.written.Target != domain.CustomerLifecycleDeleting || tx.written.DeleteNotBefore == nil || len(tx.written.Operations) != 1 {
		t.Fatalf("план удаления %+v", tx.written)
	}
}
