package postgres

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
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
	// что и показала проверка на вакуумность.
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
