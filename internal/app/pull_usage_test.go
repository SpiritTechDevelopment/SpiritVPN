package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	"github.com/RomanRyabinkin/SpiritVPN/internal/nodeagent"
)

// Юнит-тесты воркера учёта трафика. Смысл слоя — порядок фаз: порядок фаз требует,
// чтобы во время RPC не было открытой транзакции, а курсор подтверждался только
// после durable commit группы. Сами начисления, дедуп и порог живут
// в домене и SQL и проверяются там.

const (
	usageNodeID    = domain.NodeID("node-a")
	usageCustomer  = "cust-1"
	usageAccountID = "u.testaccountingid00001"
	usageSpoolID   = "spool-1"
)

// fakeUsageRepo ведёт журнал шагов: без него «RPC вне транзакции» не отличить от
// «RPC внутри неё», а «курсор не сдвинут» — от «сдвинут и откатился».
type fakeUsageRepo struct {
	journal []string

	claimed  []*app.ClaimedUsageNode
	claimErr error

	owners map[string]app.UsageOwner

	// groupErr — отказ транзакции группы. Он обязан остановить шаг ДО
	// подтверждения курсора.
	groupErr    error
	advanceErr  error
	releaseErr  error
	quarantined []app.UsageBatchRef

	cursors []nodeagent.UsageCursor

	// bootstrap — последнее записанное значение признака по нодам.
	bootstrap map[domain.NodeID]bool

	// Что видит транзакция группы.
	entitlement *domain.Entitlement
	period      *app.UsagePeriod
}

func (r *fakeUsageRepo) SetNodeNeedsBootstrap(
	_ context.Context,
	nodeID domain.NodeID,
	needsBootstrap bool,
) error {
	r.journal = append(r.journal, "bootstrap")
	if r.bootstrap == nil {
		r.bootstrap = make(map[domain.NodeID]bool)
	}
	r.bootstrap[nodeID] = needsBootstrap
	return nil
}

func (r *fakeUsageRepo) ClaimNode(
	context.Context, string, time.Duration, time.Duration,
) (*app.ClaimedUsageNode, error) {
	r.journal = append(r.journal, "claim")
	if r.claimErr != nil {
		return nil, r.claimErr
	}
	if len(r.claimed) == 0 {
		return nil, nil
	}

	node := r.claimed[0]
	r.claimed = r.claimed[1:]
	return node, nil
}

func (r *fakeUsageRepo) ReleaseNode(context.Context, domain.NodeID, string) error {
	r.journal = append(r.journal, "release")
	return r.releaseErr
}

func (r *fakeUsageRepo) ResolveAccounting(
	_ context.Context, accountingIDs []string,
) (map[string]app.UsageOwner, error) {
	r.journal = append(r.journal, "resolve")

	owners := make(map[string]app.UsageOwner, len(accountingIDs))
	for _, id := range accountingIDs {
		if owner, ok := r.owners[id]; ok {
			owners[id] = owner
		}
	}
	return owners, nil
}

func (r *fakeUsageRepo) QuarantineItems(
	_ context.Context, batch app.UsageBatchRef, _ string, _ []domain.UsageItem,
) error {
	r.journal = append(r.journal, "quarantine")
	r.quarantined = append(r.quarantined, batch)
	return nil
}

func (r *fakeUsageRepo) WithinUsageGroupTx(ctx context.Context, fn func(app.UsageGroupTx) error) error {
	r.journal = append(r.journal, "tx-begin")
	if err := fn(r); err != nil {
		return err
	}
	if r.groupErr != nil {
		return r.groupErr
	}
	r.journal = append(r.journal, "tx-commit")
	return nil
}

func (r *fakeUsageRepo) AdvanceCursor(
	_ context.Context, _ domain.NodeID, cursor nodeagent.UsageCursor,
) error {
	r.journal = append(r.journal, "advance")
	r.cursors = append(r.cursors, cursor)
	return r.advanceErr
}

// Ниже — UsageGroupTx. Репозиторий реализует его сам, как и fakeDispatchRepo:
// отдельный объект пришлось бы синхронизировать с журналом руками.

func (r *fakeUsageRepo) Now(context.Context) (time.Time, error) {
	return time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC), nil
}

func (r *fakeUsageRepo) LockEntitlement(context.Context, string) (*domain.Entitlement, error) {
	r.journal = append(r.journal, "lock-entitlement")
	return r.entitlement, nil
}

func (r *fakeUsageRepo) LockPeriodAt(context.Context, string, time.Time) (*app.UsagePeriod, error) {
	r.journal = append(r.journal, "lock-period")
	return r.period, nil
}

func (r *fakeUsageRepo) LockNodeUsage(
	context.Context, uuid.UUID, domain.NodeID,
) (domain.NodeQuotaUsage, error) {
	r.journal = append(r.journal, "lock-node-usage")
	return domain.NodeQuotaUsage{NodeID: usageNodeID}, nil
}

func (r *fakeUsageRepo) RegisterProcessed(
	_ context.Context,
	_ app.UsageBatchRef,
	_ uuid.UUID,
	_ domain.UsageItemResult,
	items []domain.UsageItem,
) ([]domain.UsageItem, error) {
	r.journal = append(r.journal, "register")
	return items, nil
}

func (r *fakeUsageRepo) LoadNodeAccesses(
	context.Context, string, domain.NodeID,
) ([]domain.Access, error) {
	r.journal = append(r.journal, "load-accesses")
	return nil, nil
}

func (r *fakeUsageRepo) WriteUsageGroup(context.Context, app.MaterializedUsageGroup) error {
	r.journal = append(r.journal, "write-group")
	return nil
}

// fakeUsageAgent отдаёт заготовленный исход и записывает, что у него спросили.
type fakeUsageAgent struct {
	outcome nodeagent.PullOutcome

	// journal общий с репозиторием: только так видно, что RPC произошёл между
	// транзакциями, а не внутри одной из них.
	journal *[]string

	acknowledged nodeagent.UsageCursor
	maxBatches   uint32
	calls        int
}

func (a *fakeUsageAgent) GetNodeState(
	_ context.Context,
	_ nodeagent.Endpoint,
	acknowledged nodeagent.UsageCursor,
	maxBatches uint32,
) nodeagent.PullOutcome {
	*a.journal = append(*a.journal, "rpc")
	a.acknowledged = acknowledged
	a.maxBatches = maxBatches
	a.calls++
	return a.outcome
}

// newPullUsage собирает воркер на общем журнале.
func newPullUsage(repo *fakeUsageRepo, agent *fakeUsageAgent) *app.PullUsage {
	agent.journal = &repo.journal

	return app.NewPullUsage(
		repo, agent, &countingIDs{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"owner-1", time.Minute, 15*time.Second,
	)
}

// claimedNode — нода с заранее подтверждённой позицией спула.
func claimedNode(cursor nodeagent.UsageCursor) *app.ClaimedUsageNode {
	return &app.ClaimedUsageNode{
		NodeID:   usageNodeID,
		Endpoint: nodeagent.Endpoint{NodeID: string(usageNodeID), Address: "10.0.0.1:9443"},
		Cursor:   cursor,
	}
}

// usageBatch — batch с одним опознаваемым item.
func usageBatch(spoolID string, sequence uint64) nodeagent.UsageBatch {
	return nodeagent.UsageBatch{
		Cursor:      nodeagent.UsageCursor{SpoolID: spoolID, Sequence: sequence},
		CollectedAt: time.Date(2026, 8, 10, 11, 59, 0, 0, time.UTC),
		Items: []nodeagent.UserUsage{
			{AccountingID: usageAccountID, UplinkBytes: 100, DownlinkBytes: 200},
		},
	}
}

// livingCustomer наполняет репозиторий так, чтобы группа доходила до записи.
func livingCustomer(repo *fakeUsageRepo) {
	repo.owners = map[string]app.UsageOwner{
		usageAccountID: {AccessID: uuid.New(), CustomerID: usageCustomer, EntryNodeID: usageNodeID},
	}
	repo.entitlement = &domain.Entitlement{FleetID: 1, ExpiresAt: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}
	repo.period = &app.UsagePeriod{
		Period: domain.QuotaPeriod{ID: uuid.New(), UsageQuotaBytes: 1 << 30},
	}
}

// steps склеивает журнал для сравнения порядка целиком, а не по одному шагу.
func steps(journal []string) string { return strings.Join(journal, ",") }

// TestPullUsageCallsAgentOutsideTransaction — держать транзакцию открытой во
// время обращения к агенту запрещено.
//
// Проверяется позицией в журнале, а не «моком, который бы заметил»: единственный
// способ отличить вызов вне транзакции от вызова внутри неё — порядок событий.
func TestPullUsageCallsAgentOutsideTransaction(t *testing.T) {
	repo := &fakeUsageRepo{claimed: []*app.ClaimedUsageNode{claimedNode(nodeagent.UsageCursor{})}}
	livingCustomer(repo)

	agent := &fakeUsageAgent{outcome: nodeagent.PullOutcome{
		State: &nodeagent.NodeState{
			NodeID:  string(usageNodeID),
			Batches: []nodeagent.UsageBatch{usageBatch(usageSpoolID, 1)},
		},
	}}

	uc := newPullUsage(repo, agent)
	if _, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	rpc, txBegin := -1, -1
	for i, step := range repo.journal {
		if step == "rpc" {
			rpc = i
		}
		if step == "tx-begin" && txBegin == -1 {
			txBegin = i
		}
	}

	if rpc == -1 || txBegin == -1 {
		t.Fatalf("журнал неполон: %s", steps(repo.journal))
	}
	if rpc > txBegin {
		t.Errorf("RPC на позиции %d произошёл после открытия транзакции на %d: %s",
			rpc, txBegin, steps(repo.journal))
	}
}

// TestPullUsageAdvancesCursorAfterCommit — курсор подтверждается только после
// commit группы. Обратный порядок означал бы, что упавшая группа
// уже подтверждена и batch к нам больше не приедет.
func TestPullUsageAdvancesCursorAfterCommit(t *testing.T) {
	repo := &fakeUsageRepo{claimed: []*app.ClaimedUsageNode{claimedNode(nodeagent.UsageCursor{})}}
	livingCustomer(repo)

	agent := &fakeUsageAgent{outcome: nodeagent.PullOutcome{
		State: &nodeagent.NodeState{
			NodeID: string(usageNodeID),
			Batches: []nodeagent.UsageBatch{
				usageBatch(usageSpoolID, 1),
				usageBatch(usageSpoolID, 2),
			},
		},
	}}

	uc := newPullUsage(repo, agent)
	if _, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	// Сравниваются Первые вхождения. Проверка «в журнале есть tx-commit,advance»
	// не проверяла бы ничего: при двух batch эта пара складывается и на стыке —
	// commit первого рядом с подтверждением второго, — то есть проходила бы и
	// при полностью обратном порядке внутри batch.
	firstCommit := slices.Index(repo.journal, "tx-commit")
	firstAdvance := slices.Index(repo.journal, "advance")

	if firstCommit == -1 || firstAdvance == -1 {
		t.Fatalf("журнал неполон: %s", steps(repo.journal))
	}
	if firstAdvance < firstCommit {
		t.Errorf("курсор подтверждён на позиции %d, до commit группы на %d: %s",
			firstAdvance, firstCommit, steps(repo.journal))
	}

	// Каждый batch подтверждается отдельно: иначе отказ на втором откатил бы и
	// уже начисленный первый.
	want := []nodeagent.UsageCursor{
		{SpoolID: usageSpoolID, Sequence: 1},
		{SpoolID: usageSpoolID, Sequence: 2},
	}
	if len(repo.cursors) != len(want) {
		t.Fatalf("подтверждений курсора %d, ожидалось %d: %v", len(repo.cursors), len(want), repo.cursors)
	}
	for i, cursor := range want {
		if repo.cursors[i] != cursor {
			t.Errorf("подтверждение %d: %+v, ожидалось %+v", i, repo.cursors[i], cursor)
		}
	}
}

// TestPullUsageKeepsCursorWhenGroupFails — главная гарантия учёта: отказ группы
// оставляет курсор на месте, batch приедет снова, а уже начисленные items
// схлопнет дедуп.
//
// Тест непустой в обе стороны: сдвинь подтверждение курсора выше обработки
// группы — и он упадёт на отсутствующей ошибке и на лишнем подтверждении.
func TestPullUsageKeepsCursorWhenGroupFails(t *testing.T) {
	repo := &fakeUsageRepo{
		claimed:  []*app.ClaimedUsageNode{claimedNode(nodeagent.UsageCursor{})},
		groupErr: errors.New("база недоступна"),
	}
	livingCustomer(repo)

	agent := &fakeUsageAgent{outcome: nodeagent.PullOutcome{
		State: &nodeagent.NodeState{
			NodeID:  string(usageNodeID),
			Batches: []nodeagent.UsageBatch{usageBatch(usageSpoolID, 1)},
		},
	}}

	uc := newPullUsage(repo, agent)

	// progressed не проверяется намеренно: runWorker разбирает err первой веткой
	// и до этого значения не доходит. Утверждение о нём закрепило бы
	// случайность реализации, а не контракт.
	if _, err := uc.ProcessNext(context.Background()); err == nil {
		t.Fatal("отказ группы не вернул ошибку, значит цикл не сделает backoff")
	}

	if len(repo.cursors) != 0 {
		t.Errorf("курсор подтверждён при отказе группы: %v", repo.cursors)
	}

	// Lease снимается и на отказе: иначе нода простаивала бы до истечения TTL,
	// который заметно длиннее интервала опроса.
	if !strings.HasSuffix(steps(repo.journal), "release") {
		t.Errorf("lease не снят после отказа: %s", steps(repo.journal))
	}
}

// TestPullUsageSkipsAcknowledgedBatch — монотонность: уже
// подтверждённый batch не должен открывать транзакцию.
func TestPullUsageSkipsAcknowledgedBatch(t *testing.T) {
	acknowledged := nodeagent.UsageCursor{SpoolID: usageSpoolID, Sequence: 5}
	repo := &fakeUsageRepo{claimed: []*app.ClaimedUsageNode{claimedNode(acknowledged)}}
	livingCustomer(repo)

	agent := &fakeUsageAgent{outcome: nodeagent.PullOutcome{
		State: &nodeagent.NodeState{
			NodeID:  string(usageNodeID),
			Batches: []nodeagent.UsageBatch{usageBatch(usageSpoolID, 5)},
		},
	}}

	uc := newPullUsage(repo, agent)
	if _, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	if strings.Contains(steps(repo.journal), "tx-begin") {
		t.Errorf("подтверждённый batch открыл транзакцию: %s", steps(repo.journal))
	}
	if len(repo.cursors) != 0 {
		t.Errorf("подтверждённый batch двинул курсор: %v", repo.cursors)
	}

	// Агенту уходит именно то, что durable закоммичено: передать больше — значит
	// разрешить ему удалить неучтённый трафик.
	if agent.acknowledged != acknowledged {
		t.Errorf("агенту передано %+v, ожидалось %+v", agent.acknowledged, acknowledged)
	}
}

// TestPullUsageResetsCursorOnNewSpool — новый spool_id нумеруется с нуля, и
// сравнивать его sequence с прежним acked_sequence нельзя.
//
// Без сброса batch №1 нового спула оказался бы «уже подтверждённым» относительно
// позиции 5 старого и был бы молча потерян.
func TestPullUsageResetsCursorOnNewSpool(t *testing.T) {
	repo := &fakeUsageRepo{claimed: []*app.ClaimedUsageNode{
		claimedNode(nodeagent.UsageCursor{SpoolID: usageSpoolID, Sequence: 5}),
	}}
	livingCustomer(repo)

	agent := &fakeUsageAgent{outcome: nodeagent.PullOutcome{
		State: &nodeagent.NodeState{
			NodeID:  string(usageNodeID),
			Batches: []nodeagent.UsageBatch{usageBatch("spool-2", 1)},
		},
	}}

	uc := newPullUsage(repo, agent)
	if _, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	want := nodeagent.UsageCursor{SpoolID: "spool-2", Sequence: 1}
	if len(repo.cursors) != 1 || repo.cursors[0] != want {
		t.Errorf("подтверждения курсора %v, ожидалось [%+v]", repo.cursors, want)
	}
}

// TestPullUsageQuarantinesBeforeGroups — карантин идёт первым и отдельной
// транзакцией: упавшая следом группа не должна оставлять плохие items
// неотмеченными, иначе они будут блокировать batch на каждом опросе.
func TestPullUsageQuarantinesBeforeGroups(t *testing.T) {
	repo := &fakeUsageRepo{claimed: []*app.ClaimedUsageNode{claimedNode(nodeagent.UsageCursor{})}}
	livingCustomer(repo)

	batch := usageBatch(usageSpoolID, 1)
	batch.Items = append(batch.Items, nodeagent.UserUsage{
		AccountingID: "u.unknown00000000000001", UplinkBytes: 1,
	})

	agent := &fakeUsageAgent{outcome: nodeagent.PullOutcome{
		State: &nodeagent.NodeState{
			NodeID:  string(usageNodeID),
			Batches: []nodeagent.UsageBatch{batch},
		},
	}}

	uc := newPullUsage(repo, agent)
	if _, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	if !strings.Contains(steps(repo.journal), "quarantine,tx-begin") {
		t.Errorf("карантин не предшествует транзакции группы: %s", steps(repo.journal))
	}
	if len(repo.quarantined) != 1 {
		t.Fatalf("карантинов %d, ожидался 1", len(repo.quarantined))
	}
	want := app.UsageBatchRef{NodeID: usageNodeID, SpoolID: usageSpoolID, Sequence: 1}
	if repo.quarantined[0] != want {
		t.Errorf("карантин помечен как %+v, ожидалось %+v", repo.quarantined[0], want)
	}
}

// TestPullUsageForeignNodeIsQuarantined — агент отчитался за юзера, которого
// backend на нём не размещал. Начислять это на опрошенную ноду нельзя: её квота
// тут ни при чём.
func TestPullUsageForeignNodeIsQuarantined(t *testing.T) {
	repo := &fakeUsageRepo{claimed: []*app.ClaimedUsageNode{claimedNode(nodeagent.UsageCursor{})}}
	livingCustomer(repo)
	repo.owners[usageAccountID] = app.UsageOwner{
		AccessID: uuid.New(), CustomerID: usageCustomer, EntryNodeID: domain.NodeID("node-b"),
	}

	agent := &fakeUsageAgent{outcome: nodeagent.PullOutcome{
		State: &nodeagent.NodeState{
			NodeID:  string(usageNodeID),
			Batches: []nodeagent.UsageBatch{usageBatch(usageSpoolID, 1)},
		},
	}}

	uc := newPullUsage(repo, agent)
	if _, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	if len(repo.quarantined) != 1 {
		t.Errorf("item чужой ноды не в карантине: %s", steps(repo.journal))
	}
	if strings.Contains(steps(repo.journal), "write-group") {
		t.Errorf("трафик чужой ноды начислен опрошенной: %s", steps(repo.journal))
	}
}

// TestPullUsageUnavailableNodeIsProgress — недоступность ноды не меняет ни
// состава fleet, ни desired state. Шаг при этом считается выполненным: темп
// повтора задаёт MinInterval, а не цикл воркера.
func TestPullUsageUnavailableNodeIsProgress(t *testing.T) {
	repo := &fakeUsageRepo{claimed: []*app.ClaimedUsageNode{claimedNode(nodeagent.UsageCursor{})}}
	agent := &fakeUsageAgent{outcome: nodeagent.PullOutcome{
		Code: nodeagent.CodeUnavailable, Message: "нода не отвечает",
	}}

	uc := newPullUsage(repo, agent)
	progressed, err := uc.ProcessNext(context.Background())

	if err != nil {
		t.Fatalf("недоступная нода вернула ошибку шага: %v", err)
	}
	if !progressed {
		t.Error("недоступная нода не засчитана прогрессом, цикл будет крутиться без пауз")
	}
	if strings.Contains(steps(repo.journal), "tx-begin") {
		t.Errorf("отказ RPC открыл транзакцию: %s", steps(repo.journal))
	}
	if !strings.HasSuffix(steps(repo.journal), "release") {
		t.Errorf("lease не снят: %s", steps(repo.journal))
	}
}

// TestPullUsageIdleWhenNothingToPoll — опрашивать нечего: ни RPC, ни lease.
func TestPullUsageIdleWhenNothingToPoll(t *testing.T) {
	repo := &fakeUsageRepo{}
	agent := &fakeUsageAgent{}

	uc := newPullUsage(repo, agent)
	progressed, err := uc.ProcessNext(context.Background())

	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if progressed {
		t.Error("пустой проход сообщил о прогрессе, из-за чего цикл не заснёт")
	}
	if agent.calls != 0 {
		t.Errorf("агент опрошен %d раз без взятой ноды", agent.calls)
	}
	if steps(repo.journal) != "claim" {
		t.Errorf("журнал %q, ожидался только claim", steps(repo.journal))
	}
}

// TestPullUsageCapsBatchesPerPull — потолок держит шаг коротким: batch содержит
// до 5 000 items, и неограниченный ответ обрабатывался бы дольше
// собственного lease.
func TestPullUsageCapsBatchesPerPull(t *testing.T) {
	repo := &fakeUsageRepo{claimed: []*app.ClaimedUsageNode{claimedNode(nodeagent.UsageCursor{})}}
	agent := &fakeUsageAgent{outcome: nodeagent.PullOutcome{
		State: &nodeagent.NodeState{NodeID: string(usageNodeID)},
	}}

	uc := newPullUsage(repo, agent)
	if _, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	if agent.maxBatches == 0 {
		t.Error("потолок batch не задан: решение о размере ответа отдано агенту")
	}
}
