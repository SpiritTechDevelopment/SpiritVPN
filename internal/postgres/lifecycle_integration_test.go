package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
)

func TestIntegrationBlockDeleteAndReapply(t *testing.T) {
	applyUC, pool := newFixture(t)
	seedTopology(t, pool, []string{"node-a"}, nil)
	repo := New(pool)
	ids := crypto.NewGenerator()
	expires := time.Now().UTC().Add(24 * time.Hour)

	if err := applyUC.Execute(context.Background(), command(1, 1<<20, expires)); err != nil {
		t.Fatalf("первый Apply: %v", err)
	}
	oldAccounting := scalar[string](t, pool,
		`SELECT accounting_id FROM vpn_accesses WHERE customer_id = $1`, testCustomerID)

	blockUC := app.NewSetCustomerAccessState(repo, ids)
	if err := blockUC.Execute(context.Background(), app.SetCustomerAccessStateCommand{
		Command: domain.AdministrativeCommand{
			CustomerID: testCustomerID, Target: domain.CustomerLifecycleBlocked, CommandNumber: 2,
		},
	}); err != nil {
		t.Fatalf("block: %v", err)
	}
	if got := scalar[string](t, pool,
		`SELECT lifecycle_state FROM customer_entitlements WHERE customer_id = $1`, testCustomerID); got != "BLOCKED" {
		t.Fatalf("lifecycle = %s", got)
	}
	if got := scalar[int64](t, pool,
		`SELECT count(*) FROM vpn_accesses WHERE customer_id = $1 AND desired_state = 'ABSENT'`, testCustomerID); got != 1 {
		t.Fatalf("ABSENT access = %d", got)
	}

	deleteUC := app.NewDeleteCustomerAccess(repo, ids, 0)
	state, err := deleteUC.Execute(context.Background(), app.DeleteCustomerAccessCommand{
		Command: domain.DeleteCommand{CustomerID: testCustomerID, CommandNumber: 3},
	})
	if err != nil || state != domain.CustomerDeletionPending {
		t.Fatalf("Delete state=%s err=%v", state, err)
	}

	// Имитируем подтверждение последней ENSURE_ABSENT агентом. Старые операции
	// уже SUPERSEDED транзакцией Delete.
	exec(t, pool, `UPDATE vpn_accesses SET apply_state = 'APPLIED' WHERE customer_id = $1`, testCustomerID)
	exec(t, pool, `UPDATE agent_operations o SET status = 'SUCCEEDED', completed_at = now(), next_attempt_at = NULL
		FROM vpn_accesses a
		WHERE a.customer_id = $1 AND o.access_id = a.access_id
		  AND o.desired_version = a.desired_version`, testCustomerID)

	progressed, err := repo.FinalizeNextDeletion(context.Background())
	if err != nil || !progressed {
		t.Fatalf("Finalize progressed=%v err=%v", progressed, err)
	}
	if got := scalar[string](t, pool,
		`SELECT lifecycle_state FROM customer_entitlements WHERE customer_id = $1`, testCustomerID); got != "DELETED" {
		t.Fatalf("lifecycle после cleanup = %s", got)
	}
	if got := scalar[int64](t, pool,
		`SELECT count(*) FROM vpn_accesses WHERE customer_id = $1`, testCustomerID); got != 0 {
		t.Fatalf("access после cleanup = %d", got)
	}

	if err := applyUC.Execute(context.Background(), command(4, 2<<20, expires.Add(24*time.Hour))); err != nil {
		t.Fatalf("Apply после DELETED: %v", err)
	}
	newAccounting := scalar[string](t, pool,
		`SELECT accounting_id FROM vpn_accesses WHERE customer_id = $1`, testCustomerID)
	if newAccounting == oldAccounting {
		t.Fatal("accounting_id был переиспользован")
	}
	if got := scalar[string](t, pool,
		`SELECT lifecycle_state FROM customer_entitlements WHERE customer_id = $1`, testCustomerID); got != "ACTIVE" {
		t.Fatalf("lifecycle после reapply = %s", got)
	}
}

func TestIntegrationSameCommandNumberConflictAcrossRPCs(t *testing.T) {
	applyUC, pool := newFixture(t)
	seedTopology(t, pool, []string{"node-a"}, nil)
	if err := applyUC.Execute(context.Background(), command(7, 1<<20, time.Now().UTC().Add(time.Hour))); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	uc := app.NewSetCustomerAccessState(New(pool), crypto.NewGenerator())
	err := uc.Execute(context.Background(), app.SetCustomerAccessStateCommand{Command: domain.AdministrativeCommand{
		CustomerID: testCustomerID, Target: domain.CustomerLifecycleBlocked, CommandNumber: 7,
	}})
	if err != domain.ErrCommandNumberConflict {
		t.Fatalf("ошибка = %v", err)
	}
}
