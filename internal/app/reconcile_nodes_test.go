package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	"github.com/RomanRyabinkin/SpiritVPN/internal/nodeagent"
)

const (
	reconcileTestTTL = time.Minute
	// reconcileTestMaxAge — предел давности снимка Xray в тестах.
	reconcileTestMaxAge = 10 * time.Minute
)

// fixedClock — часы, которые не идут: возраст наблюдения обязан задаваться
// тестом, а не тем, сколько занял прогон.
type fixedClock struct{}

func (fixedClock) Now() time.Time { return observationNow }

// observationNow — «сейчас» для сверки инвентаря.
var observationNow = time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)

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
	// observed — что отдаётся на ObserveUsers. Нулевое значение непригодно для
	// сверки, и такой агент не может случайно спровоцировать полный набор.
	observed nodeagent.InventoryOutcome

	journal      *[]string
	calls        int
	observeCalls int
	users        []nodeagent.User
}

func (a *fakeReconcileAgent) ObserveUsers(
	_ context.Context,
	_ nodeagent.Endpoint,
) nodeagent.InventoryOutcome {
	*a.journal = append(*a.journal, "observe")
	a.observeCalls++
	return a.observed
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
		// Нода после потери состояния: ей набор нужен целиком, и путь полного
		// набора она проходит без всякой сверки (§16). Тесты сверки заводят ноду
		// отдельно — им нужен как раз обратный случай.
		NeedsBootstrap: true,
		Users:          users,
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
		repo, agent, testSealer(t), crypto.NewGenerator(), fixedClock{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"worker-1", reconcileTestTTL, reconcileTestTTL, reconcileTestMaxAge)

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

// Сверка с фактическим инвентарём Xray (§16).
//
// До этого среза полный набор летел на каждую ноду каждый интервал. Теперь он
// летит только туда, где сверка нашла расхождение, — поэтому «набор не
// отправлен» стало нормальным исходом шага, и каждый способ до него дойти
// проверяется отдельно.

// settledNode — нода, взятая по таймеру, а не после потери состояния.
func settledNode(t *testing.T, users ...app.ReconcileUser) *app.ClaimedReconcileNode {
	t.Helper()

	node := reconcileNode(t, users...)
	node.NeedsBootstrap = false
	return node
}

// observedInventory — пригодный снимок с заданными юзерами.
func observedInventory(users ...nodeagent.ActualUser) nodeagent.InventoryOutcome {
	return nodeagent.InventoryOutcome{
		Inventory: &nodeagent.Inventory{
			Users:      users,
			ObservedAt: observationNow.Add(-time.Minute),
			Complete:   true,
		},
		Code: nodeagent.CodeApplied,
	}
}

// sentUser восстанавливает, каким юзер уехал бы агенту: сверять инвентарь надо
// с расшифрованным набором, а не с тем, что лежит в БД.
func sentUser(t *testing.T, node *app.ClaimedReconcileNode) []nodeagent.ActualUser {
	t.Helper()

	// Набор собирает сам use case, поэтому берём его тем же путём: прогоняем шаг
	// по ноде с bootstrap, где сверки нет, и смотрим, что доехало до агента.
	probe := reconcileNode(t, node.Users...)
	harness, _, agent := newReconcileHarness(t, probe, reconcileApplied())
	if _, err := harness.ProcessNext(context.Background()); err != nil {
		t.Fatalf("подготовка набора: %v", err)
	}

	actual := make([]nodeagent.ActualUser, 0, len(agent.users))
	for _, user := range agent.users {
		actual = append(actual, nodeagent.ActualUser{User: user, BackendManaged: true})
	}
	return actual
}

// TestReconcileSkipsFullSetWhenNodeMatches — §16: совпавшая нода не получает
// полного набора. Это и есть смысл сверки: набор дорогой, а расхождения нет.
func TestReconcileSkipsFullSetWhenNodeMatches(t *testing.T) {
	node := settledNode(t, reconcileUser(t, "acc-1"), reconcileUser(t, "acc-2"))
	uc, repo, agent := newReconcileHarness(t, node, reconcileApplied())
	agent.observed = observedInventory(sentUser(t, node)...)

	progressed, err := uc.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if !progressed {
		t.Fatal("шаг не засчитан прогрессом, хотя нода была взята и сверена")
	}

	if agent.observeCalls != 1 {
		t.Errorf("наблюдений инвентаря %d, ожидалось 1", agent.observeCalls)
	}
	if agent.calls != 0 {
		t.Errorf("полный набор отправлен %d раз, ожидалось 0 — нода совпадает с desired", agent.calls)
	}
	// Результата не было, принимать нечего: AcceptReconcile отмечает операции
	// удовлетворёнными, а здесь агенту ничего не поручали.
	if slices.Contains(repo.journal, "accept") {
		t.Errorf("результат принят, хотя набор не отправлялся: %v", repo.journal)
	}
}

// TestReconcileSendsFullSetOnDrift — §16: найденное расхождение чинится полным
// набором (решение 86).
func TestReconcileSendsFullSetOnDrift(t *testing.T) {
	node := settledNode(t, reconcileUser(t, "acc-1"), reconcileUser(t, "acc-2"))
	uc, _, agent := newReconcileHarness(t, node, reconcileApplied())

	// На ноде остался только первый: второй пропал.
	agent.observed = observedInventory(sentUser(t, node)[:1]...)

	if _, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	if agent.calls != 1 {
		t.Fatalf("полный набор отправлен %d раз, ожидался 1", agent.calls)
	}
	// Чинит набор по desired state, а не по инвентарю: на ноду уезжают ОБА
	// юзера, включая того, который там уже есть.
	if got := len(agent.users); got != 2 {
		t.Errorf("в наборе %d юзеров, ожидалось 2 — набор собран из инвентаря, а не из desired", got)
	}
}

// TestReconcileSkipsFullSetOnUnusableInventory — решение 88: непригодный снимок
// отменяет сверку целиком.
//
// Чиним мы полным набором, который удаляет по определению, поэтому любой триггер
// по такому снимку был бы выводом удаления в обход запрета §16.
func TestReconcileSkipsFullSetOnUnusableInventory(t *testing.T) {
	for _, tc := range []struct {
		name     string
		observed nodeagent.InventoryOutcome
	}{
		{
			name: "снимок усечён",
			observed: nodeagent.InventoryOutcome{
				Inventory: &nodeagent.Inventory{ObservedAt: observationNow, Complete: false},
				Code:      nodeagent.CodeApplied,
			},
		},
		{
			name: "наблюдение протухло",
			observed: nodeagent.InventoryOutcome{
				Inventory: &nodeagent.Inventory{
					ObservedAt: observationNow.Add(-reconcileTestMaxAge - time.Second),
					Complete:   true,
				},
				Code: nodeagent.CodeApplied,
			},
		},
		{
			name:     "агент недоступен",
			observed: nodeagent.InventoryOutcome{Code: nodeagent.CodeUnavailable},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Набор заведомо разошёлся бы с пустым инвентарём: если бы отбраковка
			// не сработала, полный набор ушёл бы, и тест это увидит.
			node := settledNode(t, reconcileUser(t, "acc-1"))
			uc, _, agent := newReconcileHarness(t, node, reconcileApplied())
			agent.observed = tc.observed

			progressed, err := uc.ProcessNext(context.Background())
			if err != nil {
				t.Fatalf("ProcessNext: %v", err)
			}
			if !progressed {
				t.Error("шаг не засчитан прогрессом: нода была взята, и повторять её немедленно незачем")
			}
			if agent.calls != 0 {
				t.Errorf("полный набор отправлен %d раз, ожидалось 0", agent.calls)
			}
		})
	}
}

// TestReconcileBootstrapSkipsInventory — §16: ноде с потерянным состоянием набор
// нужен целиком, и сверяться с ней не о чем.
//
// Агент с needs_bootstrap не имеет права удалять юзеров и сам из этого состояния
// не выйдет. Спрашивать у него инвентарь значило бы тратить лишний RPC на то,
// чтобы всё равно отправить полный набор.
func TestReconcileBootstrapSkipsInventory(t *testing.T) {
	node := reconcileNode(t, reconcileUser(t, "acc-1"))
	uc, _, agent := newReconcileHarness(t, node, reconcileApplied())
	// Инвентарь совпадает с набором — и всё равно не должен помешать.
	agent.observed = observedInventory(sentUser(t, node)...)

	if _, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	if agent.observeCalls != 0 {
		t.Errorf("наблюдений инвентаря %d, ожидалось 0: bootstrap сверки не требует", agent.observeCalls)
	}
	if agent.calls != 1 {
		t.Errorf("полный набор отправлен %d раз, ожидался 1", agent.calls)
	}
}
