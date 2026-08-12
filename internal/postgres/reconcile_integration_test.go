package postgres

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	"github.com/RomanRyabinkin/SpiritVPN/internal/nodeagent"
)

// Интеграционные тесты authoritative reconcile (§10). Смысл слоя в SQL: предикат
// захвата, состав набора «фактически разрешённых» юзеров и гейт по
// desired_revision, который решает, принимать ли результат.

const (
	testReconcileOwner = "reconcile-integration"
	// testReconcileInterval заведомо длиннее теста: срабатывание таймера в тестах
	// сделало бы предикат захвата неотличимым от «берём что попало».
	testReconcileInterval = time.Hour
	testReconcileTTL      = time.Minute
	// testMaxObservationAge — предел давности снимка Xray. Заведомо длиннее
	// прогона: тест, где наблюдение протухает само по себе, проверял бы часы, а
	// не сверку.
	testMaxObservationAge = time.Hour
)

// claimForReconcile берёт ноду напрямую через репозиторий.
//
// Через репозиторий, а не use case: проверяется предикат захвата и состав
// набора, то есть ровно то, что живёт в SQL.
func claimForReconcile(t *testing.T, stack usageStack, owner string) *app.ClaimedReconcileNode {
	t.Helper()

	node, err := New(stack.pool).ClaimNodeForReconcile(
		context.Background(), owner, testReconcileTTL, testReconcileInterval)
	if err != nil {
		t.Fatalf("ClaimNodeForReconcile: %v", err)
	}
	return node
}

// settleReconcile делает вид, что все ноды только что reconcile-ились: иначе
// нода с NULL в reconcile_attempted_at берётся всегда, и проверять предикат
// становится нечем.
func settleReconcile(t *testing.T, stack usageStack) {
	t.Helper()
	exec(t, stack.pool,
		`UPDATE vpn_nodes SET reconcile_attempted_at = now(), reconcile_lease_owner = NULL,
		                      reconcile_lease_expires_at = NULL`)
}

func accountingIDs(node *app.ClaimedReconcileNode) []string {
	ids := make([]string, 0, len(node.Users))
	for _, user := range node.Users {
		ids = append(ids, user.AccountingID)
	}
	return ids
}

// TestIntegrationReconcileClaimsNeverReconciledNode — нода, которую не
// reconcile-или ни разу, берётся вперёд всех: у неё нет и не может быть
// подтверждения, что её набор кто-то когда-то приводил в порядок.
func TestIntegrationReconcileClaimsNeverReconciledNode(t *testing.T) {
	stack := newUsageStack(t)
	seedUsageCustomer(t, stack, 1<<30)

	if node := claimForReconcile(t, stack, testReconcileOwner); node == nil {
		t.Fatal("нода с reconcile_attempted_at IS NULL не взята")
	}
}

// TestIntegrationReconcileSkipsSettledNode — таймер не сработал, bootstrap не
// нужен: брать нечего. Без этого утверждения предикат захвата был бы неотличим
// от «берём любую текущую ноду».
func TestIntegrationReconcileSkipsSettledNode(t *testing.T) {
	stack := newUsageStack(t)
	seedUsageCustomer(t, stack, 1<<30)
	settleReconcile(t, stack)

	if node := claimForReconcile(t, stack, testReconcileOwner); node != nil {
		t.Fatalf("взята нода %s, хотя интервал не истёк и bootstrap не нужен", node.NodeID)
	}
}

// TestIntegrationReconcileClaimsBootstrapNode — §10: needs_bootstrap берёт ноду
// вне очереди. Агент с новым или повреждённым локальным состоянием не имеет
// права удалять юзеров и сам из этого состояния не выйдет, поэтому ждать
// таймера здесь нельзя.
func TestIntegrationReconcileClaimsBootstrapNode(t *testing.T) {
	stack := newUsageStack(t)
	seedUsageCustomer(t, stack, 1<<30)
	settleReconcile(t, stack)

	if err := New(stack.pool).SetNodeNeedsBootstrap(context.Background(), "NL-1", true); err != nil {
		t.Fatalf("SetNodeNeedsBootstrap: %v", err)
	}

	node := claimForReconcile(t, stack, testReconcileOwner)
	if node == nil {
		t.Fatal("нода с needs_bootstrap не взята: bootstrap не является поводом")
	}
	if node.NodeID != "NL-1" {
		t.Errorf("взята нода %s, ожидалась NL-1", node.NodeID)
	}
}

// TestIntegrationReconcileSetExcludesExpiredAndExhausted — §10: истёкшие
// entitlement и исчерпавшие квоту в набор не входят, ДАЖЕ если expiry или usage
// worker ещё не перевели их access в ABSENT.
//
// Это не оптимизация: набор авторитетен, поэтому включить такого юзера значило
// бы вернуть на ноду доступ, который уже не оплачен.
func TestIntegrationReconcileSetExcludesExpiredAndExhausted(t *testing.T) {
	stack := newUsageStack(t)
	accountingID := seedUsageCustomer(t, stack, 1<<30)

	// Обе ноды фикстуры не reconcile-ились ни разу, поэтому очередь между ними
	// произвольна; здесь нужна именно NL-1, на которой лежит access.
	releaseAndReclaim(t, stack)

	node := claimForReconcile(t, stack, testReconcileOwner)
	if node == nil || node.NodeID != "NL-1" {
		t.Fatalf("нода NL-1 не взята: %v", node)
	}
	// На NL-1 два access одного customer: FREEDOM и BRIDGE, у которого NL-1 —
	// входная нода. Оба backend-owned, оба обязаны быть в наборе.
	if got := accountingIDs(node); len(got) != 2 || !slices.Contains(got, accountingID) {
		t.Fatalf("набор %v, ожидались оба access NL-1, включая %s", got, accountingID)
	}
	// Доступы всё ещё PRESENT — исключать их будет не смена desired_state.
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_accesses WHERE entry_node_id = 'NL-1' AND desired_state = 'PRESENT'`); got != 2 {
		t.Fatalf("PRESENT-access на NL-1 %d, ожидалось 2", got)
	}

	// Фаза 1: entitlement истёк, но expiry worker ещё не отработал.
	exec(t, stack.pool, `UPDATE customer_entitlements SET expires_at = now() - interval '1 minute'`)
	releaseAndReclaim(t, stack)

	node = claimForReconcile(t, stack, testReconcileOwner)
	if node == nil {
		t.Fatal("нода не взята после истечения entitlement")
	}
	if got := accountingIDs(node); len(got) != 0 {
		t.Errorf("набор %v, ожидался пустой: истёкший entitlement в него не входит", got)
	}

	// Фаза 2: срок вернули, зато квота ноды исчерпана.
	exec(t, stack.pool, `UPDATE customer_entitlements SET expires_at = now() + interval '30 days'`)
	exec(t, stack.pool,
		`INSERT INTO node_quota_usage (quota_period_id, node_id, exhausted_at)
		 SELECT p.quota_period_id, 'NL-1', now() FROM quota_periods p WHERE p.closed_at IS NULL
		 ON CONFLICT (quota_period_id, node_id) DO UPDATE SET exhausted_at = now()`)
	releaseAndReclaim(t, stack)

	node = claimForReconcile(t, stack, testReconcileOwner)
	if node == nil {
		t.Fatal("нода не взята после исчерпания квоты")
	}
	if got := accountingIDs(node); len(got) != 0 {
		t.Errorf("набор %v, ожидался пустой: исчерпавший квоту в него не входит", got)
	}
}

// releaseAndReclaim возвращает ноды в состояние «пора reconcile-ить».
func releaseAndReclaim(t *testing.T, stack usageStack) {
	t.Helper()
	exec(t, stack.pool,
		`UPDATE vpn_nodes SET reconcile_attempted_at = NULL, reconcile_lease_owner = NULL,
		                      reconcile_lease_expires_at = NULL WHERE node_id = 'NL-1'`)
	exec(t, stack.pool,
		`UPDATE vpn_nodes SET reconcile_attempted_at = now() WHERE node_id <> 'NL-1'`)
}

// TestIntegrationReconcileAcceptMarksAppliedAndClosesOperations — §10: принятый
// результат отмечает desired states применёнными и завершает операции, которые
// полный набор уже удовлетворил.
func TestIntegrationReconcileAcceptMarksAppliedAndClosesOperations(t *testing.T) {
	stack := newUsageStack(t)
	seedUsageCustomer(t, stack, 1<<30)

	// Истечение переводит access в ABSENT и создаёт Remove-операции. Доставлять их
	// диспетчером не будем: их удовлетворит полный набор, в котором этих юзеров
	// уже нет, — и это ровно тот случай, ради которого §10 велит завершать
	// удовлетворённые операции.
	exec(t, stack.pool, `UPDATE customer_entitlements SET expires_at = now() - interval '1 minute'`)
	drainExpiry(t, app.NewExpireCustomers(New(stack.pool), crypto.NewGenerator()))

	pending := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations WHERE node_id = 'NL-1' AND status IN ('PENDING', 'RETRY_WAIT')`)
	if pending == 0 {
		t.Fatal("истечение не создало ожидающих операций: тесту нечего закрывать")
	}

	releaseAndReclaim(t, stack)
	node := claimForReconcile(t, stack, testReconcileOwner)
	if node == nil {
		t.Fatal("нода NL-1 не взята")
	}

	applied := make([]uuid.UUID, 0, len(node.Users))
	for _, user := range node.Users {
		applied = append(applied, user.AccessID)
	}

	accepted, err := New(stack.pool).AcceptReconcile(context.Background(), app.ReconcileAcceptance{
		NodeID:           node.NodeID,
		DesiredRevision:  node.DesiredRevision,
		AppliedAccessIDs: applied,
	})
	if err != nil {
		t.Fatalf("AcceptReconcile: %v", err)
	}
	if !accepted {
		t.Fatal("результат не принят, хотя desired_revision не менялась")
	}

	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_accesses
		 WHERE entry_node_id = 'NL-1' AND retired_at IS NULL AND apply_state <> 'APPLIED'`); got != 0 {
		t.Errorf("не отмечено применёнными %d access на NL-1", got)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations
		 WHERE node_id = 'NL-1' AND status IN ('PENDING', 'RETRY_WAIT')`); got != 0 {
		t.Errorf("осталось %d ожидающих операций: reconcile их уже удовлетворил", got)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT reconciled_revision FROM vpn_nodes WHERE node_id = 'NL-1'`); got != node.DesiredRevision {
		t.Errorf("reconciled_revision %d, ожидалась %d", got, node.DesiredRevision)
	}
}

// TestIntegrationReconcileRejectsStaleRevision — §10: если desired_revision
// сдвинулась, пока набор был на проводе, результат не принимается и НИЧЕГО не
// пишет. Набор на ноде уже не тот, который заказан.
func TestIntegrationReconcileRejectsStaleRevision(t *testing.T) {
	stack := newUsageStack(t)
	seedUsageCustomer(t, stack, 1<<30)

	releaseAndReclaim(t, stack)
	node := claimForReconcile(t, stack, testReconcileOwner)
	if node == nil {
		t.Fatal("нода NL-1 не взята")
	}

	// Пока набор «в пути», desired state ноды уехал.
	exec(t, stack.pool,
		`UPDATE vpn_nodes SET desired_revision = desired_revision + 1 WHERE node_id = 'NL-1'`)
	exec(t, stack.pool,
		`UPDATE vpn_accesses SET apply_state = 'PENDING' WHERE entry_node_id = 'NL-1'`)

	accepted, err := New(stack.pool).AcceptReconcile(context.Background(), app.ReconcileAcceptance{
		NodeID:          node.NodeID,
		DesiredRevision: node.DesiredRevision,
	})
	if err != nil {
		t.Fatalf("AcceptReconcile: %v", err)
	}
	if accepted {
		t.Fatal("принят результат устаревшего набора")
	}

	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_accesses WHERE entry_node_id = 'NL-1' AND apply_state = 'APPLIED'`); got != 0 {
		t.Errorf("отмечено применёнными %d access при непринятом результате", got)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT reconciled_revision FROM vpn_nodes WHERE node_id = 'NL-1'`); got != 0 {
		t.Errorf("reconciled_revision %d, ожидался 0: результат не принимался", got)
	}
}

// TestIntegrationReconcileLeaseIsExclusive — §10 и §13: пока нода взята, второй
// воркер её не получает. Два полных набора на одну ноду одновременно означали бы
// гонку двух авторитетных истин.
func TestIntegrationReconcileLeaseIsExclusive(t *testing.T) {
	stack := newUsageStack(t)
	seedUsageCustomer(t, stack, 1<<30)

	// Все ноды «только что reconcile-ились», и лишь NL-1 остаётся годной — по
	// bootstrap, который захват не сбрасывает.
	//
	// Это существенно, а не декорация. Захват сам ставит reconcile_attempted_at, и
	// без bootstrap нода переставала бы проходить условие ТАЙМЕРА сразу после
	// первого же захвата: второй возвращал бы пусто независимо от lease, и тест
	// мерил бы таймер вместо исключительности. Ровно так он и был написан сперва,
	// что и показал mutation testing.
	settleReconcile(t, stack)
	if err := New(stack.pool).SetNodeNeedsBootstrap(context.Background(), "NL-1", true); err != nil {
		t.Fatalf("SetNodeNeedsBootstrap: %v", err)
	}

	first := claimForReconcile(t, stack, testReconcileOwner)
	if first == nil || first.NodeID != "NL-1" {
		t.Fatalf("нода NL-1 не взята: %v", first)
	}

	if second := claimForReconcile(t, stack, "reconcile-other"); second != nil {
		t.Fatalf("вторая попытка получила ноду %s при живом lease", second.NodeID)
	}

	if err := New(stack.pool).ReleaseNodeReconcile(
		context.Background(), first.NodeID, testReconcileOwner); err != nil {
		t.Fatalf("ReleaseNodeReconcile: %v", err)
	}

	again := claimForReconcile(t, stack, "reconcile-other")
	if again == nil {
		t.Fatal("после снятия lease нода не досталась другому владельцу")
	}
	if again.NodeID != "NL-1" {
		t.Errorf("после снятия lease досталась нода %s, ожидалась NL-1", again.NodeID)
	}
}

// Сверка с фактическим инвентарём Xray (§10). Смысл здесь не в SQL, а в стыке:
// desired-набор читается из БД и расшифровывается, а сравнивается с тем, что
// «показала» нода. Юнит-тест сверки этот стык проверить не может — он оперирует
// уже готовыми наборами.

// inventoryAgent отдаёт заготовленный инвентарь и запоминает полные наборы.
type inventoryAgent struct {
	observed     nodeagent.InventoryOutcome
	observeCalls int

	users []nodeagent.User
	calls int
}

func (a *inventoryAgent) ObserveUsers(
	_ context.Context,
	_ nodeagent.Endpoint,
) nodeagent.InventoryOutcome {
	a.observeCalls++
	return a.observed
}

func (a *inventoryAgent) ReconcileUsers(
	_ context.Context,
	_ nodeagent.Endpoint,
	_ string,
	users []nodeagent.User,
) nodeagent.ReconcileResult {
	a.calls++
	a.users = users
	return nodeagent.ReconcileResult{
		Outcome:   nodeagent.Outcome{Result: domain.AttemptSucceeded, Code: nodeagent.CodeApplied},
		Unchanged: uint32(len(users)),
	}
}

// newInventoryStack собирает reconcile поверх настоящего репозитория.
func newInventoryStack(t *testing.T, stack usageStack) (*app.ReconcileNodes, *inventoryAgent) {
	t.Helper()

	agent := &inventoryAgent{}
	uc := app.NewReconcileNodes(
		New(stack.pool), agent, testCipher(t), crypto.NewGenerator(), app.SystemClock{},
		testLogger(stack.logs), testReconcileOwner, testReconcileTTL, testReconcileInterval,
		testMaxObservationAge)

	return uc, agent
}

// bootstrapNode готовит ровно одну ноду к захвату и прогоняет по ней полный
// набор. Что уехало агенту, остаётся в inventoryAgent.users.
//
// Через bootstrap, а не в обход: так набор для сравнения получен тем же путём,
// каким его собирает бой, и тест не изобретает собственного представления о
// desired state. Остальные ноды отмечаются сверенными, поэтому захват
// детерминирован — интервал в тестах длиннее прогона.
func bootstrapNode(t *testing.T, stack usageStack, uc *app.ReconcileNodes, nodeID string) {
	t.Helper()

	settleReconcile(t, stack)
	exec(t, stack.pool, `UPDATE vpn_nodes SET needs_bootstrap = true WHERE node_id = $1`, nodeID)

	if progressed, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("bootstrap ноды %s: %v", nodeID, err)
	} else if !progressed {
		t.Fatalf("нода %s не взята на bootstrap", nodeID)
	}

	// Агент отчитался, что состояние больше не потеряно, — в бою это делает
	// usage-воркер по ответу GetNodeState.
	exec(t, stack.pool, `UPDATE vpn_nodes SET needs_bootstrap = false WHERE node_id = $1`, nodeID)
	exec(t, stack.pool, `UPDATE vpn_nodes SET reconcile_attempted_at = NULL WHERE node_id = $1`, nodeID)
}

// observedFrom собирает пригодный снимок из юзеров, которые реально уехали на ноду.
func observedFrom(users []nodeagent.User) nodeagent.InventoryOutcome {
	actual := make([]nodeagent.ActualUser, 0, len(users))
	for _, user := range users {
		actual = append(actual, nodeagent.ActualUser{User: user, BackendManaged: true})
	}

	return nodeagent.InventoryOutcome{
		Inventory: &nodeagent.Inventory{
			Users:      actual,
			ObservedAt: time.Now().UTC(),
			Complete:   true,
		},
		Code: nodeagent.CodeApplied,
	}
}

// TestIntegrationReconcileSkipsFullSetWhenInventoryMatches — §10: нода, чей
// инвентарь совпал с desired state, полного набора не получает.
//
// Это главный эффект среза: до него набор летел на каждую ноду каждый интервал.
func TestIntegrationReconcileSkipsFullSetWhenInventoryMatches(t *testing.T) {
	stack := newUsageStack(t)
	seedUsageCustomer(t, stack, 1<<30)
	uc, agent := newInventoryStack(t, stack)

	bootstrapNode(t, stack, uc, "NL-1")
	if agent.calls != 1 {
		t.Fatalf("подготовка: полных наборов %d, ожидался 1", agent.calls)
	}
	// На NL-1 два access: FREEDOM и BRIDGE, где она входная (§4).
	if got := len(agent.users); got != 2 {
		t.Fatalf("подготовка: в наборе %d юзеров, ожидалось 2", got)
	}
	agent.observed = observedFrom(agent.users)

	if progressed, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	} else if !progressed {
		t.Fatal("нода не взята на сверку")
	}

	if agent.observeCalls != 1 {
		t.Errorf("наблюдений инвентаря %d, ожидалось 1", agent.observeCalls)
	}
	if agent.calls != 1 {
		t.Errorf("полных наборов %d, ожидался 1 — сверка не удержала лишний набор", agent.calls)
	}
}

// TestIntegrationReconcileSendsFullSetOnInventoryDrift — §10: расхождение с
// фактическим состоянием чинится полным набором (решение 86).
func TestIntegrationReconcileSendsFullSetOnInventoryDrift(t *testing.T) {
	stack := newUsageStack(t)
	seedUsageCustomer(t, stack, 1<<30)
	uc, agent := newInventoryStack(t, stack)

	bootstrapNode(t, stack, uc, "NL-1")
	// Один из двух юзеров с ноды пропал.
	agent.observed = observedFrom(agent.users[:1])
	sent := agent.users

	if _, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	if agent.calls != 2 {
		t.Fatalf("полных наборов %d, ожидалось 2: расхождение не починено", agent.calls)
	}
	// Чинит по desired state из БД, а не по инвентарю: уезжают оба юзера.
	if got := len(agent.users); got != len(sent) {
		t.Errorf("в наборе %d юзеров, ожидалось %d — набор собран из инвентаря", got, len(sent))
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_nodes WHERE node_id = 'NL-1' AND reconciled_revision IS NOT NULL`); got != 1 {
		t.Error("результат reconcile не принят")
	}
}

// TestIntegrationReconcileIgnoresForeignNamespace — §10: инфраструктурные юзеры
// на ноде расхождением не являются.
//
// Агент не удаляет их даже при complete-наборе, поэтому реагировать на них
// значило бы гонять полный набор каждый интервал до скончания века — то самое
// вечное «расхождение», которое ничем не чинится.
func TestIntegrationReconcileIgnoresForeignNamespace(t *testing.T) {
	stack := newUsageStack(t)
	seedUsageCustomer(t, stack, 1<<30)
	uc, agent := newInventoryStack(t, stack)

	bootstrapNode(t, stack, uc, "NL-1")

	observed := observedFrom(agent.users)
	observed.Inventory.Users = append(observed.Inventory.Users, nodeagent.ActualUser{
		User:           nodeagent.User{AccountingID: "infra.probe"},
		BackendManaged: false,
	})
	agent.observed = observed

	if _, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	if agent.calls != 1 {
		t.Errorf("полных наборов %d, ожидался 1 — чужой юзер принят за расхождение", agent.calls)
	}
}
