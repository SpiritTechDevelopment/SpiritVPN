package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	"github.com/RomanRyabinkin/SpiritVPN/internal/nodeagent"
)

const reconcileTestTTL = time.Minute

// fakeReconcileRepo ведёт журнал шагов: без него «набор отправлен и не принят» не
// отличить от «не отправлен вовсе», а снятие lease — от его отсутствия.
type fakeReconcileRepo struct {
	journal []string

	claimed  []*app.ClaimedReconcileNode
	claimErr error

	// accepted — что вернёт AcceptReconcile: false означает, что desired_revision
	// ушла вперёд, пока набор был на проводе.
	accepted   bool
	acceptErr  error
	acceptance *app.ReconcileAcceptance
}

func (r *fakeReconcileRepo) ClaimNodeForReconcile(
	context.Context, string, time.Duration, time.Duration,
) (*app.ClaimedReconcileNode, error) {
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

func (r *fakeReconcileRepo) ReleaseNodeReconcile(context.Context, domain.NodeID, string) error {
	r.journal = append(r.journal, "release")
	return nil
}

func (r *fakeReconcileRepo) AcceptReconcile(
	_ context.Context,
	acceptance app.ReconcileAcceptance,
) (bool, error) {
	r.journal = append(r.journal, "accept")
	r.acceptance = &acceptance
	return r.accepted, r.acceptErr
}

// fakeReconcileAgent запоминает набор, который до него доехал.
type fakeReconcileAgent struct {
	result nodeagent.ReconcileResult

	journal *[]string
	calls   int
	users   []nodeagent.User
}

func (a *fakeReconcileAgent) ReconcileUsers(
	_ context.Context,
	_ nodeagent.Endpoint,
	_ string,
	users []nodeagent.User,
) nodeagent.ReconcileResult {
	*a.journal = append(*a.journal, "rpc")
	a.calls++
	a.users = users
	return a.result
}

func reconcileNode(t *testing.T, users ...app.ReconcileUser) *app.ClaimedReconcileNode {
	t.Helper()

	return &app.ClaimedReconcileNode{
		NodeID: "NL-1",
		Endpoint: nodeagent.Endpoint{
			NodeID: "NL-1", Address: "nl-1:9443",
			TLSServerName: "nl-1", CertificateIdentity: "nl-1",
		},
		Flow:            domain.FlowXTLSRprxVision,
		DesiredRevision: 7,
		Users:           users,
	}
}

func reconcileUser(t *testing.T, accountingID string) app.ReconcileUser {
	t.Helper()

	return app.ReconcileUser{
		AccessID:     uuid.New(),
		AccountingID: accountingID,
		EgressKey:    "",
		Credential:   sealTestUUID(t),
	}
}

func newReconcileHarness(
	t *testing.T,
	node *app.ClaimedReconcileNode,
	result nodeagent.ReconcileResult,
) (*app.ReconcileNodes, *fakeReconcileRepo, *fakeReconcileAgent) {
	t.Helper()

	repo := &fakeReconcileRepo{accepted: true}
	if node != nil {
		repo.claimed = []*app.ClaimedReconcileNode{node}
	}
	agent := &fakeReconcileAgent{result: result, journal: &repo.journal}

	uc := app.NewReconcileNodes(
		repo, agent, testSealer(t), crypto.NewGenerator(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"worker-1", reconcileTestTTL, reconcileTestTTL)

	return uc, repo, agent
}

func reconcileApplied() nodeagent.ReconcileResult {
	return nodeagent.ReconcileResult{
		Outcome: nodeagent.Outcome{Result: domain.AttemptSucceeded, Code: nodeagent.CodeApplied},
	}
}

// TestReconcileSendsFullSet — §10: агенту уезжает весь набор ноды, а принятый
// результат отмечает применёнными ровно те access, что в нём были.
func TestReconcileSendsFullSet(t *testing.T) {
	node := reconcileNode(t, reconcileUser(t, "acc-1"), reconcileUser(t, "acc-2"))
	uc, repo, agent := newReconcileHarness(t, node, reconcileApplied())

	progressed, err := uc.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if !progressed {
		t.Fatal("шаг не признан прогрессом")
	}

	if len(agent.users) != 2 {
		t.Fatalf("до агента доехало %d юзеров, ожидалось 2", len(agent.users))
	}
	// Открытый uuid обязан быть настоящим: подмена Sealer скрыла бы ровно то, что
	// проверяется — на ноду уезжает расшифрованный credential.
	if agent.users[0].ClientUUID.Reveal() != testClientUUID.Reveal() {
		t.Error("на ноду уехал не тот client_uuid")
	}
	if agent.users[0].Flow != domain.FlowXTLSRprxVision {
		t.Errorf("flow %q, ожидался из public_config ноды", agent.users[0].Flow)
	}

	if repo.acceptance == nil {
		t.Fatal("результат не принят")
	}
	if repo.acceptance.DesiredRevision != 7 {
		t.Errorf("принята ревизия %d, ожидалась зафиксированная 7", repo.acceptance.DesiredRevision)
	}
	if len(repo.acceptance.AppliedAccessIDs) != 2 {
		t.Errorf("применённых access %d, ожидалось 2", len(repo.acceptance.AppliedAccessIDs))
	}
}

// TestReconcileSendsEmptySet — §10 и §18: пустой набор легален и означает
// «backend-owned юзеров на ноде нет». Не отправить его значило бы оставить на
// ноде всех, кого backend уже снял.
func TestReconcileSendsEmptySet(t *testing.T) {
	uc, _, agent := newReconcileHarness(t, reconcileNode(t), reconcileApplied())

	if _, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	if agent.calls != 1 {
		t.Fatalf("вызовов агента %d, ожидался 1: пустой набор не отправлен", agent.calls)
	}
	if len(agent.users) != 0 {
		t.Errorf("до агента доехало %d юзеров, ожидался пустой набор", len(agent.users))
	}
}

// TestReconcileAbortsOnUnreadableCredential — самое важное утверждение среза.
//
// Набор авторитетен: юзер, которого в нём нет, будет с ноды УДАЛЁН. Поэтому
// нерасшифровавшийся credential обязан остановить весь шаг, а не быть пропущен, —
// иначе отказ ключа превратился бы в снятие рабочего доступа нашими руками.
func TestReconcileAbortsOnUnreadableCredential(t *testing.T) {
	broken := reconcileUser(t, "acc-broken")
	broken.Credential = crypto.SealedCredential{Blob: []byte("не расшифруется"), KeyID: "test-key"}

	node := reconcileNode(t, reconcileUser(t, "acc-1"), broken)
	uc, repo, agent := newReconcileHarness(t, node, reconcileApplied())

	progressed, err := uc.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if !progressed {
		t.Error("шаг не признан прогрессом: нода была взята и отпущена")
	}

	if agent.calls != 0 {
		t.Fatalf("агент вызван %d раз: на ноду уехал НЕПОЛНЫЙ набор, и она удалит "+
			"юзера, чей credential просто не прочитался", agent.calls)
	}
	if repo.acceptance != nil {
		t.Error("результат принят, хотя набор не отправлялся")
	}
	// Lease обязан сняться и на этом пути, иначе нода простаивала бы до истечения TTL.
	if !containsStep(repo.journal, "release") {
		t.Error("lease не снят после отказа расшифрования")
	}
}

// TestReconcileSkipsNodeWithBrokenPublicConfig — пустой flow означает
// неразобравшийся public_config. Отправить набор с чужим flow — сломать на ноде
// всех разом, поэтому она пропускается целиком.
func TestReconcileSkipsNodeWithBrokenPublicConfig(t *testing.T) {
	node := reconcileNode(t, reconcileUser(t, "acc-1"))
	node.Flow = ""

	uc, _, agent := newReconcileHarness(t, node, reconcileApplied())

	if _, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if agent.calls != 0 {
		t.Errorf("агент вызван %d раз при непригодном public_config", agent.calls)
	}
}

// TestReconcileIgnoresStaleResult — §10: результат не принимается, если
// desired_revision сдвинулась, пока набор был на проводе. Это не ошибка шага:
// более новое состояние уже доставляют обычные Ensure.
func TestReconcileIgnoresStaleResult(t *testing.T) {
	uc, repo, _ := newReconcileHarness(t, reconcileNode(t, reconcileUser(t, "acc-1")), reconcileApplied())
	repo.accepted = false

	progressed, err := uc.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("устаревший результат стал ошибкой шага: %v", err)
	}
	if !progressed {
		t.Error("шаг не признан прогрессом")
	}
}

// TestReconcileUnavailableNodeChangesNothing — §16: недоступность агента не
// доходит до записи. Принимать нечего — набор не применялся.
func TestReconcileUnavailableNodeChangesNothing(t *testing.T) {
	uc, repo, _ := newReconcileHarness(t, reconcileNode(t, reconcileUser(t, "acc-1")),
		nodeagent.ReconcileResult{
			Outcome: nodeagent.Outcome{
				Result: domain.AttemptRetryable, Code: nodeagent.CodeUnavailable,
			},
		})

	progressed, err := uc.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("недоступность ноды стала ошибкой шага: %v", err)
	}
	if !progressed {
		t.Error("шаг не признан прогрессом: темп задаёт интервал, а не цикл")
	}
	if repo.acceptance != nil {
		t.Error("результат принят, хотя агент не применил набор")
	}
	if !containsStep(repo.journal, "release") {
		t.Error("lease не снят после недоступности ноды")
	}
}

func TestReconcileNothingToDo(t *testing.T) {
	uc, repo, agent := newReconcileHarness(t, nil, reconcileApplied())

	progressed, err := uc.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if progressed {
		t.Error("пустой проход признан прогрессом: цикл не уснёт")
	}
	if agent.calls != 0 {
		t.Error("агент вызван без взятой ноды")
	}
	// Снимать нечего: lease не брался.
	if containsStep(repo.journal, "release") {
		t.Error("снят lease, который не брался")
	}
}

func TestReconcileClaimFailureIsStepFailure(t *testing.T) {
	uc, repo, _ := newReconcileHarness(t, nil, reconcileApplied())
	repo.claimErr = errors.New("база недоступна")

	progressed, err := uc.ProcessNext(context.Background())
	if err == nil {
		t.Fatal("отказ базы не стал ошибкой шага: цикл пойдёт дальше без backoff")
	}
	if progressed {
		t.Error("отказавший шаг признан прогрессом")
	}
}

// containsStep — есть ли шаг в журнале.
func containsStep(journal []string, step string) bool {
	for _, entry := range journal {
		if entry == step {
			return true
		}
	}
	return false
}
