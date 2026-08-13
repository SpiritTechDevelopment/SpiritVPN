package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
)

// Юнит-тесты воркера истечения. Смысл слоя — порядок шагов и то, что план
// строится от expires_at, прочитанного под locком: выборка воркера могла устареть,
// пока он ждал блокировку.

// fakeExpiryTx ведёт журнал шагов и отдаёт заготовленное состояние.
type fakeExpiryTx struct {
	journal []string

	due      *app.ExpiredCustomer
	dueErr   error
	accesses []domain.Access

	written  []app.MaterializedExpiryPlan
	writeErr error
	audits   []app.AuditEvent
}

func (tx *fakeExpiryTx) record(step string) { tx.journal = append(tx.journal, step) }

func (tx *fakeExpiryTx) Now(context.Context) (time.Time, error) {
	tx.record("now")
	return testNow, nil
}

func (tx *fakeExpiryTx) LockNextDueCustomer(context.Context) (*app.ExpiredCustomer, error) {
	tx.record("lock-due")
	return tx.due, tx.dueErr
}

func (tx *fakeExpiryTx) LoadAccesses(context.Context, string) ([]domain.Access, error) {
	tx.record("load-accesses")
	return tx.accesses, nil
}

func (tx *fakeExpiryTx) WriteExpiry(_ context.Context, plan app.MaterializedExpiryPlan) error {
	tx.record("write")
	if tx.writeErr != nil {
		return tx.writeErr
	}
	tx.written = append(tx.written, plan)
	return nil
}

func (tx *fakeExpiryTx) AppendAudit(_ context.Context, event app.AuditEvent) error {
	tx.record("audit")
	tx.audits = append(tx.audits, event)
	return nil
}

type fakeExpiryRepo struct {
	tx        *fakeExpiryTx
	committed bool
}

func (r *fakeExpiryRepo) WithinExpiryTx(ctx context.Context, fn func(app.ExpiryTx) error) error {
	if err := fn(r.tx); err != nil {
		return err
	}
	r.committed = true
	return nil
}

const expiryTestCustomer = "customer-1"

// expiredDue — истёкший час назад customer.
func expiredDue() *app.ExpiredCustomer {
	return &app.ExpiredCustomer{
		CustomerID: expiryTestCustomer,
		Entitlement: domain.Entitlement{
			FleetID:        1,
			ExpiresAt:      testNow.Add(-time.Hour),
			DesiredVersion: 2,
		},
	}
}

func presentAccess(node domain.NodeID) domain.Access {
	return domain.Access{
		ID:               uuid.New(),
		Kind:             domain.AccessKindFreedom,
		LogicalTargetKey: domain.LogicalTargetKey(node),
		Generation:       1,
		EntryNodeID:      node,
		DesiredState:     domain.DesiredStatePresent,
		DesiredVersion:   4,
	}
}

func newExpiryHarness(tx *fakeExpiryTx) (*app.ExpireCustomers, *fakeExpiryRepo) {
	repo := &fakeExpiryRepo{tx: tx}
	return app.NewExpireCustomers(repo, &countingIDs{}), repo
}

// TestExpireRevokesAccessAndAudits — воркер переводит access в ABSENT и
// создаёт Remove в одной транзакции; истечение журналируется.
func TestExpireRevokesAccessAndAudits(t *testing.T) {
	tx := &fakeExpiryTx{
		due:      expiredDue(),
		accesses: []domain.Access{presentAccess("node-a"), presentAccess("node-b")},
	}
	uc, repo := newExpiryHarness(tx)

	progressed, err := uc.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if !progressed {
		t.Fatal("снятие доступа не засчитано прогрессом")
	}
	if !repo.committed {
		t.Fatal("транзакция не закоммичена")
	}

	// Порядок нормативен: время, lock корневой строки, чтение, запись, аудит.
	want := []string{"now", "lock-due", "load-accesses", "write", "audit"}
	if !equalSteps(tx.journal, want) {
		t.Fatalf("порядок шагов %v, ожидался %v", tx.journal, want)
	}

	if len(tx.written) != 1 {
		t.Fatalf("записей плана %d, ожидалась 1", len(tx.written))
	}
	plan := tx.written[0]

	if len(plan.Plan.DesiredChanges) != 2 {
		t.Errorf("погашено access %d, ожидалось 2", len(plan.Plan.DesiredChanges))
	}
	// Каждому погашенному access — своя операция удаления.
	if len(plan.Operations) != len(plan.Plan.DesiredChanges) {
		t.Fatalf("операций %d, изменений %d", len(plan.Operations), len(plan.Plan.DesiredChanges))
	}
	for i, operation := range plan.Operations {
		change := plan.Plan.DesiredChanges[i]
		if operation.DesiredState != domain.DesiredStateAbsent {
			t.Errorf("операция %d не ABSENT: %s", i, operation.DesiredState)
		}
		if operation.AccessID != change.AccessID || operation.NodeID != change.EntryNodeID {
			t.Errorf("операция %d не соответствует изменению", i)
		}
		if operation.DesiredVersion != change.DesiredVersion {
			t.Errorf("версия операции %d, у изменения %d", operation.DesiredVersion, change.DesiredVersion)
		}
	}

	if len(tx.audits) != 1 {
		t.Fatalf("записей аудита %d, ожидалась 1", len(tx.audits))
	}
	audit := tx.audits[0]
	if audit.TargetID != expiryTestCustomer {
		t.Errorf("target %q, ожидался %q", audit.TargetID, expiryTestCustomer)
	}
	if audit.Metadata["revoked_access"] != 2 {
		t.Errorf("в аудите revoked_access = %v, ожидалось 2", audit.Metadata["revoked_access"])
	}
}

// TestExpireSparesRenewedCustomer — expires_at перечитывается под locком,
// поэтому уже закоммиченный renewal отменяет снятие. Ни записи, ни аудита.
func TestExpireSparesRenewedCustomer(t *testing.T) {
	due := expiredDue()
	due.Entitlement.ExpiresAt = testNow.Add(24 * time.Hour)

	tx := &fakeExpiryTx{due: due, accesses: []domain.Access{presentAccess("node-a")}}
	uc, _ := newExpiryHarness(tx)

	progressed, err := uc.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if !progressed {
		t.Error("обработанный продлённый customer — это прогресс: следующая выборка его не найдёт")
	}
	if len(tx.written) != 0 {
		t.Error("expiry снял доступ у продлённого customer")
	}
	if len(tx.audits) != 0 {
		t.Error("аудит записан без снятия доступа")
	}
}

// TestExpireIdleReportsNoProgress — гасить некого, циклу пора подождать.
func TestExpireIdleReportsNoProgress(t *testing.T) {
	tx := &fakeExpiryTx{}
	uc, _ := newExpiryHarness(tx)

	progressed, err := uc.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if progressed {
		t.Error("пустая выборка засчитана прогрессом")
	}
	// Читать access не нужно, если customer не выбран.
	if !equalSteps(tx.journal, []string{"now", "lock-due"}) {
		t.Errorf("лишние шаги на холостом проходе: %v", tx.journal)
	}
}

// TestExpireAlreadyRevokedWritesNothing — повторный проход не создаёт вторых
// Remove. В базе такой customer отсекается выборкой, но воркер обязан выдержать и
// его: между выборкой и locком его мог погасить конкурент.
func TestExpireAlreadyRevokedWritesNothing(t *testing.T) {
	absent := presentAccess("node-a")
	absent.DesiredState = domain.DesiredStateAbsent

	tx := &fakeExpiryTx{due: expiredDue(), accesses: []domain.Access{absent}}
	uc, _ := newExpiryHarness(tx)

	if _, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if len(tx.written) != 0 {
		t.Errorf("повторный проход записал план: %+v", tx.written)
	}
}

// TestExpirePropagatesErrors — отказ базы обязан дойти до цикла и откатить
// транзакцию.
func TestExpirePropagatesErrors(t *testing.T) {
	broken := errors.New("база недоступна")

	t.Run("выборка", func(t *testing.T) {
		tx := &fakeExpiryTx{dueErr: broken}
		uc, repo := newExpiryHarness(tx)

		if _, err := uc.ProcessNext(context.Background()); !errors.Is(err, broken) {
			t.Fatalf("ошибка %v, ожидалась %v", err, broken)
		}
		if repo.committed {
			t.Error("транзакция закоммичена после отказа")
		}
	})

	t.Run("запись", func(t *testing.T) {
		tx := &fakeExpiryTx{
			due:      expiredDue(),
			accesses: []domain.Access{presentAccess("node-a")},
			writeErr: broken,
		}
		uc, repo := newExpiryHarness(tx)

		if _, err := uc.ProcessNext(context.Background()); !errors.Is(err, broken) {
			t.Fatalf("ошибка %v, ожидалась %v", err, broken)
		}
		if repo.committed {
			t.Error("транзакция закоммичена после отказа")
		}
		// Аудит не должен уехать без записи, которую он описывает.
		if len(tx.audits) != 0 {
			t.Error("аудит записан при провалившейся записи плана")
		}
	})
}
