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

// Тесты воркера проверяют то, чего не видно ни в домене, ни в SQL: границу шага.
// Один шаг — один customer, курсор двигается в той же транзакции, а конец обхода
// закрывает джобу.

const testLeaseTTL = time.Minute

// fakeMaterializeTx ведёт журнал вызовов и отдаёт заранее заданное состояние.
type fakeMaterializeTx struct {
	journal []string

	job       *app.MaterializationJob
	nextID    string
	claimErr  error
	writeErr  error
	cursorSet string

	entitlement *domain.Entitlement
	period      *domain.QuotaPeriod
	usage       []domain.NodeQuotaUsage
	topology    domain.FleetTopology
	accesses    []domain.Access
	liveNodes   []domain.NodeID

	written    *app.MaterializedManifestPlan
	completed  bool
	claimOwner string
	claimTTL   time.Duration
}

func (tx *fakeMaterializeTx) record(step string) { tx.journal = append(tx.journal, step) }

func (tx *fakeMaterializeTx) Now(context.Context) (time.Time, error) {
	tx.record("Now")
	return testNow, nil
}

func (tx *fakeMaterializeTx) ClaimJob(
	_ context.Context,
	owner string,
	ttl time.Duration,
) (*app.MaterializationJob, error) {
	tx.record("ClaimJob")
	tx.claimOwner, tx.claimTTL = owner, ttl
	return tx.job, tx.claimErr
}

func (tx *fakeMaterializeTx) NextCustomer(_ context.Context, after string) (string, error) {
	tx.record("NextCustomer")
	if after == tx.nextID {
		return "", nil
	}
	return tx.nextID, nil
}

func (tx *fakeMaterializeTx) AdvanceCursor(_ context.Context, _ int64, customerID string) error {
	tx.record("AdvanceCursor")
	tx.cursorSet = customerID
	return nil
}

func (tx *fakeMaterializeTx) CompleteJob(context.Context, int64) error {
	tx.record("CompleteJob")
	tx.completed = true
	return nil
}

func (tx *fakeMaterializeTx) LockEntitlement(context.Context, string) (*domain.Entitlement, error) {
	tx.record("LockEntitlement")
	return tx.entitlement, nil
}

func (tx *fakeMaterializeTx) LockOpenQuotaPeriod(context.Context, string) (*domain.QuotaPeriod, error) {
	tx.record("LockOpenQuotaPeriod")
	return tx.period, nil
}

func (tx *fakeMaterializeTx) LockNodeQuotaUsage(context.Context, uuid.UUID) ([]domain.NodeQuotaUsage, error) {
	tx.record("LockNodeQuotaUsage")
	return tx.usage, nil
}

func (tx *fakeMaterializeTx) LoadTopology(context.Context, int64) (domain.FleetTopology, error) {
	tx.record("LoadTopology")
	return tx.topology, nil
}

func (tx *fakeMaterializeTx) LoadAccesses(context.Context, string) ([]domain.Access, error) {
	tx.record("LoadAccesses")
	return tx.accesses, nil
}

func (tx *fakeMaterializeTx) LoadLiveNodes(context.Context) ([]domain.NodeID, error) {
	tx.record("LoadLiveNodes")
	return tx.liveNodes, nil
}

func (tx *fakeMaterializeTx) WriteMaterialization(_ context.Context, plan app.MaterializedManifestPlan) error {
	tx.record("WriteMaterialization")
	tx.written = &plan
	return tx.writeErr
}

type fakeMaterializeRepo struct {
	tx        *fakeMaterializeTx
	committed bool
}

func (r *fakeMaterializeRepo) WithinMaterializationTx(
	ctx context.Context,
	fn func(app.MaterializationTx) error,
) error {
	if err := fn(r.tx); err != nil {
		return err
	}
	r.committed = true
	return nil
}

// materializeFixture — customer, у которого во fleet появилась новая нода.
func materializeFixture() *fakeMaterializeTx {
	return &fakeMaterializeTx{
		job:         &app.MaterializationJob{Revision: 7},
		nextID:      "cust-1",
		entitlement: &domain.Entitlement{FleetID: 10, ExpiresAt: testNow.Add(24 * time.Hour), DesiredVersion: 3},
		period:      &domain.QuotaPeriod{ID: uuid.New(), UsageQuotaBytes: 1 << 30},
		topology:    domain.FleetTopology{FleetID: 10, Nodes: []domain.NodeID{"NL-1"}},
		liveNodes:   []domain.NodeID{"NL-1"},
	}
}

func newMaterializeHarness(tx *fakeMaterializeTx) (*app.MaterializeManifest, *fakeMaterializeRepo) {
	repo := &fakeMaterializeRepo{tx: tx}
	uc := app.NewMaterializeManifest(repo, &countingIDs{}, &stubSealer{}, "worker-1", testLeaseTTL)
	return uc, repo
}

// TestProcessNextMaterializesOneCustomer — решение 30: шаг обрабатывает ровно
// одного customer и двигает курсор в той же транзакции (решение 34).
func TestProcessNextMaterializesOneCustomer(t *testing.T) {
	tx := materializeFixture()
	uc, repo := newMaterializeHarness(tx)

	progressed, err := uc.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if !progressed {
		t.Fatal("шаг не сообщил о прогрессе")
	}
	if !repo.committed {
		t.Fatal("транзакция не закоммичена")
	}

	if tx.cursorSet != "cust-1" {
		t.Errorf("курсор %q, ожидался cust-1", tx.cursorSet)
	}
	if tx.written == nil || len(tx.written.NewAccesses) != 1 {
		t.Fatalf("записанный план %+v, ожидался один новый access", tx.written)
	}
	if len(tx.written.Operations) != 1 {
		t.Errorf("операций %d, ожидалась 1", len(tx.written.Operations))
	}

	// Порядок чтений совпадает с нормативным порядком блокировок §11.1.
	want := []string{
		"ClaimJob", "NextCustomer", "Now",
		"LockEntitlement", "LockOpenQuotaPeriod", "LockNodeQuotaUsage",
		"LoadTopology", "LoadAccesses", "LoadLiveNodes",
		"WriteMaterialization", "AdvanceCursor",
	}
	if len(tx.journal) != len(want) {
		t.Fatalf("журнал %v,\nожидался %v", tx.journal, want)
	}
	for i, step := range want {
		if tx.journal[i] != step {
			t.Fatalf("журнал %v,\nожидался %v", tx.journal, want)
		}
	}
}

// TestProcessNextPassesLease — lease берётся от имени этой реплики и на
// заданный срок (§13, §15).
func TestProcessNextPassesLease(t *testing.T) {
	tx := materializeFixture()
	uc, _ := newMaterializeHarness(tx)

	if _, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if tx.claimOwner != "worker-1" || tx.claimTTL != testLeaseTTL {
		t.Fatalf("lease взят как %q на %s", tx.claimOwner, tx.claimTTL)
	}
}

// TestProcessNextNoJob — работы нет: шаг не сообщает о прогрессе, чтобы цикл
// подождал, и ничего не читает.
func TestProcessNextNoJob(t *testing.T) {
	tx := materializeFixture()
	tx.job = nil
	uc, _ := newMaterializeHarness(tx)

	progressed, err := uc.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if progressed {
		t.Fatal("шаг сообщил о прогрессе, хотя джоб нет")
	}
	if len(tx.journal) != 1 {
		t.Fatalf("журнал %v, ожидался только ClaimJob", tx.journal)
	}
}

// TestProcessNextCompletesJob — обход дошёл до конца: джоба закрывается, ничего
// больше не читается.
func TestProcessNextCompletesJob(t *testing.T) {
	tx := materializeFixture()
	tx.job = &app.MaterializationJob{Revision: 7, Cursor: "cust-1"}
	uc, _ := newMaterializeHarness(tx)

	progressed, err := uc.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if !progressed || !tx.completed {
		t.Fatalf("джоба не закрыта: progressed=%v completed=%v", progressed, tx.completed)
	}
	if tx.written != nil {
		t.Error("на завершении обхода записан план")
	}
}

// TestProcessNextSkipsConsistentCustomer — согласованный customer не порождает ни
// одной записи, но курсор всё равно двигается: иначе обход встал бы на месте.
func TestProcessNextSkipsConsistentCustomer(t *testing.T) {
	tx := materializeFixture()
	tx.accesses = []domain.Access{{
		ID: uuid.New(), Kind: domain.AccessKindFreedom, LogicalTargetKey: "NL-1",
		Generation: 1, EntryNodeID: "NL-1",
		DesiredState: domain.DesiredStatePresent, DesiredVersion: 1,
	}}
	tx.usage = []domain.NodeQuotaUsage{{NodeID: "NL-1", TotalBytes: 0}}
	uc, _ := newMaterializeHarness(tx)

	if _, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	if tx.written != nil {
		t.Errorf("для согласованного customer записан план: %+v", tx.written)
	}
	if tx.cursorSet != "cust-1" {
		t.Errorf("курсор %q: обход обязан двигаться и на пустом шаге", tx.cursorSet)
	}
}

// TestProcessNextPropagatesWriteError — отказ записи откатывает транзакцию
// целиком вместе с курсором: шаг обязан быть повторяемым.
func TestProcessNextPropagatesWriteError(t *testing.T) {
	writeFailed := errors.New("нет связи с базой")
	tx := materializeFixture()
	tx.writeErr = writeFailed
	uc, repo := newMaterializeHarness(tx)

	progressed, err := uc.ProcessNext(context.Background())
	if !errors.Is(err, writeFailed) {
		t.Fatalf("ошибка %v, ожидался проброс %v", err, writeFailed)
	}
	if progressed {
		t.Error("шаг с ошибкой сообщил о прогрессе")
	}
	if repo.committed {
		t.Error("транзакция закоммичена после отказа записи")
	}
}

// TestProcessNextRequiresOpenPeriod — нарушение инварианта §11 доезжает наружу,
// а не проглатывается воркером.
func TestProcessNextRequiresOpenPeriod(t *testing.T) {
	tx := materializeFixture()
	tx.period = nil
	uc, _ := newMaterializeHarness(tx)

	if _, err := uc.ProcessNext(context.Background()); !errors.Is(err, domain.ErrOpenPeriodMissing) {
		t.Fatalf("ошибка %v, ожидалась ErrOpenPeriodMissing", err)
	}
}

// materializedAccess — существующий согласованный access на указанной ноде.
func materializedAccess(id uuid.UUID, kind domain.AccessKind, target string, node domain.NodeID, egress string) domain.Access {
	return domain.Access{
		ID: id, Kind: kind, LogicalTargetKey: domain.LogicalTargetKey(target),
		Generation: 1, EntryNodeID: node, EgressKey: egress,
		DesiredState: domain.DesiredStatePresent, DesiredVersion: 1,
	}
}

// TestProcessNextIssuesRepointOperation — §6: repoint доставляется обычным
// EnsureUserPresent, и агент переиздаёт правило по новому egress_key.
func TestProcessNextIssuesRepointOperation(t *testing.T) {
	bridgeID := uuid.New()

	tx := materializeFixture()
	tx.topology = domain.FleetTopology{
		FleetID: 10,
		Nodes:   []domain.NodeID{"NL-1"},
		Bridges: []domain.BridgeRoute{{
			RoutingKey: "nl-1.to-de-1", EntryNodeID: "NL-1", ExitNodeID: "DE-1",
			EgressTag: "de-exit-v2",
		}},
	}
	tx.accesses = []domain.Access{
		materializedAccess(uuid.New(), domain.AccessKindFreedom, "NL-1", "NL-1", ""),
		materializedAccess(bridgeID, domain.AccessKindBridge, "nl-1.to-de-1", "NL-1", "de-exit"),
	}
	tx.usage = []domain.NodeQuotaUsage{{NodeID: "NL-1"}}

	uc, _ := newMaterializeHarness(tx)
	if _, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	if tx.written == nil || len(tx.written.Plan.Repoints) != 1 {
		t.Fatalf("записанный план %+v, ожидался один repoint", tx.written)
	}
	if len(tx.written.Operations) != 1 {
		t.Fatalf("операций %d, ожидалась 1", len(tx.written.Operations))
	}

	operation := tx.written.Operations[0]
	if operation.AccessID != bridgeID {
		t.Errorf("операция выпущена не на смещённый access")
	}
	if operation.DesiredState != domain.DesiredStatePresent {
		t.Errorf("desired_state операции %s, ожидался PRESENT", operation.DesiredState)
	}
	if len(tx.written.NewAccesses) != 0 {
		t.Error("repoint создал новый access")
	}
}

// TestProcessNextRetireOperationsFollowNodeLiveness — §6: удаление доставляется
// только на живую ноду; у глобально удалённой операции не выпускаются.
func TestProcessNextRetireOperationsFollowNodeLiveness(t *testing.T) {
	liveID, deadID := uuid.New(), uuid.New()

	tx := materializeFixture()
	// Топология опустела: NL-1 осталась во fleet, DE-1 исчезла отовсюду.
	tx.topology = domain.FleetTopology{FleetID: 10, Nodes: []domain.NodeID{"NL-1"}}
	tx.liveNodes = []domain.NodeID{"NL-1"}
	tx.accesses = []domain.Access{
		materializedAccess(liveID, domain.AccessKindBridge, "nl-1.to-de-1", "NL-1", "de-exit"),
		materializedAccess(deadID, domain.AccessKindFreedom, "DE-1", "DE-1", ""),
		materializedAccess(uuid.New(), domain.AccessKindFreedom, "NL-1", "NL-1", ""),
	}
	tx.usage = []domain.NodeQuotaUsage{{NodeID: "NL-1"}, {NodeID: "DE-1"}}

	uc, _ := newMaterializeHarness(tx)
	if _, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	if len(tx.written.Plan.Retire) != 2 {
		t.Fatalf("ретайрится %d access, ожидалось 2", len(tx.written.Plan.Retire))
	}
	if len(tx.written.Operations) != 1 {
		t.Fatalf("операций %d, ожидалась 1: только на живую ноду", len(tx.written.Operations))
	}

	operation := tx.written.Operations[0]
	if operation.AccessID != liveID {
		t.Error("операция выпущена не на access живой ноды")
	}
	if operation.DesiredState != domain.DesiredStateAbsent {
		t.Errorf("desired_state операции %s, ожидался ABSENT", operation.DesiredState)
	}
	if operation.NodeID != "NL-1" {
		t.Errorf("операция адресована ноде %s", operation.NodeID)
	}
}
