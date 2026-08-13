package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
)

// Интеграционные тесты воркера истечения. Смысл слоя в SQL: выборка due customer
// под FOR UPDATE SKIP LOCKED, порядок записи и то, что повторный проход ничего не
// добавляет.

// expiryStack — истечение вместе с соседними use case: доступ надо сначала
// завести, а потом убедиться, что снятие доехало до агента.
type expiryStack struct {
	manifest    *app.ApplyFleetManifest
	customer    *app.ApplyCustomerAccess
	materialize *app.MaterializeManifest
	expiry      *app.ExpireCustomers
	dispatch    *app.DispatchOperations
	links       *app.GetCustomerAccessLinks
	agent       *scriptedAgent
	pool        *pgxpool.Pool
}

func newExpiryStack(t *testing.T) expiryStack {
	t.Helper()

	customer, pool := newFixture(t)
	cipher := testCipher(t)
	repo := New(pool)
	agent := &scriptedAgent{fallback: agentApplied()}

	return expiryStack{
		manifest: app.NewApplyFleetManifest(repo),
		customer: customer,
		materialize: app.NewMaterializeManifest(
			repo, crypto.NewGenerator(), cipher, testWorkerOwner, time.Minute),
		expiry:   app.NewExpireCustomers(repo, crypto.NewGenerator()),
		dispatch: app.NewDispatchOperations(repo, agent, cipher, zeroJitter{}, testDispatchOwner, time.Minute),
		links:    app.NewGetCustomerAccessLinks(repo, cipher),
		agent:    agent,
		pool:     pool,
	}
}

// seedActiveCustomer заводит топологию и действующего customer с доставленным
// доступом: все три access в PRESENT/APPLIED.
func seedActiveCustomer(t *testing.T, stack expiryStack) {
	t.Helper()

	applyManifest(t, stack.manifest, manifestFixture(7), false)

	expiresAt := time.Now().UTC().Add(30 * 24 * time.Hour)
	if err := stack.customer.Execute(context.Background(), command(1, 1<<30, expiresAt)); err != nil {
		t.Fatalf("ApplyCustomerAccess: %v", err)
	}
	drainMaterialization(t, stack.materialize)
	drainDispatch(t, stack.dispatch)

	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_accesses WHERE desired_state = 'PRESENT' AND apply_state = 'APPLIED'`); got != seedOperationCount {
		t.Fatalf("подготовка: доставленных access %d, ожидалось %d", got, seedOperationCount)
	}
}

// expireCustomer отматывает срок в прошлое, минуя ApplyCustomerAccess: сокращение
// expires_at командой запрещено доменом.
func expireCustomer(t *testing.T, stack expiryStack) {
	t.Helper()

	exec(t, stack.pool,
		`UPDATE customer_entitlements SET expires_at = now() - interval '1 minute' WHERE customer_id = $1`,
		testCustomerID)
}

// drainExpiry крутит воркер до исчерпания работы и возвращает число шагов.
func drainExpiry(t *testing.T, uc *app.ExpireCustomers) int {
	t.Helper()

	steps := 0
	for range 100 {
		progressed, err := uc.ProcessNext(context.Background())
		if err != nil {
			t.Fatalf("ProcessNext на шаге %d: %v", steps, err)
		}
		if !progressed {
			return steps
		}
		steps++
	}

	t.Fatal("воркер истечения не сошёлся за 100 шагов")
	return steps
}

// TestIntegrationExpiryRevokesAccessEndToEnd — истечение переводит access в
// ABSENT и создаёт Remove; диспетчер доставляет их до агента.
func TestIntegrationExpiryRevokesAccessEndToEnd(t *testing.T) {
	stack := newExpiryStack(t)
	seedActiveCustomer(t, stack)
	expireCustomer(t, stack)

	if got := drainExpiry(t, stack.expiry); got != 1 {
		t.Fatalf("шагов истечения %d, ожидался 1: один customer — один шаг", got)
	}

	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_accesses WHERE desired_state <> 'ABSENT'`); got != 0 {
		t.Errorf("access не в ABSENT: %d", got)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations
		 WHERE operation_type = 'ENSURE_ABSENT' AND status = 'PENDING'`); got != seedOperationCount {
		t.Errorf("операций удаления %d, ожидалось %d", got, seedOperationCount)
	}

	// Ссылки уже заблокированы по времени, независимо от доставки.
	links, err := stack.links.Execute(context.Background(), testCustomerID)
	if err != nil {
		t.Fatalf("GetCustomerAccessLinks: %v", err)
	}
	for _, link := range links {
		if link.Status.Reason != domain.BlockReasonTimeExpired {
			t.Errorf("причина блокировки %q, ожидалась TIME_EXPIRED", link.Status.Reason)
		}
		if link.URI != "" {
			t.Error("истёкшая ссылка отдана с URI")
		}
	}

	// Снятие доезжает до агента тем же диспетчером, без отдельного пути.
	drainDispatch(t, stack.dispatch)

	if got := len(stack.agent.absent); got != seedOperationCount {
		t.Errorf("агент получил %d удалений, ожидалось %d", got, seedOperationCount)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_accesses WHERE apply_state <> 'APPLIED'`); got != 0 {
		t.Errorf("снятие не подтверждено: %d access не в APPLIED", got)
	}
}

// TestIntegrationExpiryIsIdempotent — повторный запуск не создаёт вторых
// Remove operations.
func TestIntegrationExpiryIsIdempotent(t *testing.T) {
	stack := newExpiryStack(t)
	seedActiveCustomer(t, stack)
	expireCustomer(t, stack)

	drainExpiry(t, stack.expiry)
	operationsAfterFirst := scalar[int64](t, stack.pool, `SELECT count(*) FROM agent_operations`)
	versionAfterFirst := scalar[int64](t, stack.pool,
		`SELECT desired_version FROM customer_entitlements WHERE customer_id = $1`, testCustomerID)

	// Второй проход обязан не найти работы вовсе: истёкший customer без PRESENT
	// access в выборку не попадает, иначе воркер крутился бы вхолостую.
	if got := drainExpiry(t, stack.expiry); got != 0 {
		t.Fatalf("шагов на повторном проходе %d, ожидалось 0", got)
	}
	if got := scalar[int64](t, stack.pool, `SELECT count(*) FROM agent_operations`); got != operationsAfterFirst {
		t.Errorf("операций стало %d, было %d", got, operationsAfterFirst)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT desired_version FROM customer_entitlements WHERE customer_id = $1`,
		testCustomerID); got != versionAfterFirst {
		t.Errorf("версия корневой строки сдвинулась на пустом проходе: %d, было %d", got, versionAfterFirst)
	}
}

// TestIntegrationExpiryBumpsNodeRevisionOnce — транзакция увеличивает
// desired_revision ноды ровно один раз, сколько бы access на ней ни гасло.
//
// У NL-1 их два: FREEDOM самой ноды и BRIDGE, где она входная.
func TestIntegrationExpiryBumpsNodeRevisionOnce(t *testing.T) {
	stack := newExpiryStack(t)
	seedActiveCustomer(t, stack)

	before := scalar[int64](t, stack.pool,
		`SELECT desired_revision FROM vpn_nodes WHERE node_id = 'NL-1'`)
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_accesses WHERE entry_node_id = 'NL-1' AND desired_state = 'PRESENT'`); got != 2 {
		t.Fatalf("подготовка: на NL-1 %d access, ожидалось 2", got)
	}

	expireCustomer(t, stack)
	drainExpiry(t, stack.expiry)

	if got := scalar[int64](t, stack.pool,
		`SELECT desired_revision FROM vpn_nodes WHERE node_id = 'NL-1'`); got != before+1 {
		t.Errorf("desired_revision %d, ожидалась %d: два погашенных access дали два инкремента", got, before+1)
	}
}

// TestIntegrationExpirySupersedesPendingPresent — недоставленный
// EnsureUserPresent прежней версии обязан стать SUPERSEDED, иначе на ноду уехала
// бы команда, ставящая юзера обратно уже после снятия.
func TestIntegrationExpirySupersedesPendingPresent(t *testing.T) {
	stack := newExpiryStack(t)
	applyManifest(t, stack.manifest, manifestFixture(7), false)

	expiresAt := time.Now().UTC().Add(30 * 24 * time.Hour)
	if err := stack.customer.Execute(context.Background(), command(1, 1<<30, expiresAt)); err != nil {
		t.Fatalf("ApplyCustomerAccess: %v", err)
	}
	drainMaterialization(t, stack.materialize)

	// Диспетчер намеренно не запускался: PRESENT-операции висят недоставленными.
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations WHERE operation_type = 'ENSURE_PRESENT' AND status = 'PENDING'`); got != seedOperationCount {
		t.Fatalf("подготовка: PENDING-операций %d, ожидалось %d", got, seedOperationCount)
	}

	expireCustomer(t, stack)
	drainExpiry(t, stack.expiry)

	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations
		 WHERE operation_type = 'ENSURE_PRESENT' AND status <> 'SUPERSEDED'`); got != 0 {
		t.Errorf("недоставленных PRESENT-операций осталось %d", got)
	}

	// На агента уходят только удаления.
	drainDispatch(t, stack.dispatch)
	if len(stack.agent.present) != 0 {
		t.Errorf("после истечения агенту уехало %d команд PRESENT", len(stack.agent.present))
	}
	if len(stack.agent.absent) != seedOperationCount {
		t.Errorf("агент получил %d удалений, ожидалось %d", len(stack.agent.absent), seedOperationCount)
	}
}

// TestIntegrationExpirySkipsLockedCustomer — FOR UPDATE SKIP LOCKED, поэтому
// вторая реплика не ждёт занятого customer, а сразу берёт следующего.
func TestIntegrationExpirySkipsLockedCustomer(t *testing.T) {
	stack := newExpiryStack(t)
	seedActiveCustomer(t, stack)
	expireCustomer(t, stack)
	ctx := context.Background()

	// Конкурент держит корневую строку единственного due customer.
	rival, err := stack.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = rival.Rollback(ctx) }()

	if _, err := rival.Exec(ctx,
		`SELECT customer_id FROM customer_entitlements WHERE customer_id = $1 FOR UPDATE`,
		testCustomerID); err != nil {
		t.Fatalf("подготовка конкурента: %v", err)
	}

	// Воркер обязан вернуться немедленно и с пустыми руками, а не встать в очередь.
	done := make(chan bool, 1)
	go func() {
		progressed, err := stack.expiry.ProcessNext(ctx)
		if err != nil {
			t.Errorf("ProcessNext: %v", err)
		}
		done <- progressed
	}()

	select {
	case progressed := <-done:
		if progressed {
			t.Error("воркер обработал customer, заблокированного конкурентом")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("воркер ждёт на занятой строке вместо SKIP LOCKED")
	}

	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_accesses WHERE desired_state = 'ABSENT'`); got != 0 {
		t.Errorf("доступ снят вопреки чужому locку: %d access в ABSENT", got)
	}
}

// TestIntegrationExpiryWritesAudit — истечение customer обязано попадать в
// журнал аудита.
func TestIntegrationExpiryWritesAudit(t *testing.T) {
	stack := newExpiryStack(t)
	seedActiveCustomer(t, stack)
	expireCustomer(t, stack)

	drainExpiry(t, stack.expiry)

	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM audit_events WHERE action = 'CUSTOMER_EXPIRED' AND target_id = $1`,
		testCustomerID); got != 1 {
		t.Fatalf("записей аудита %d, ожидалась 1", got)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT (sanitized_metadata->>'revoked_access')::bigint
		 FROM audit_events WHERE action = 'CUSTOMER_EXPIRED'`); got != seedOperationCount {
		t.Errorf("в аудите revoked_access = %d, ожидалось %d", got, seedOperationCount)
	}
	// Секретов в журнале быть не должно.
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM audit_events a JOIN vpn_accesses v ON true
		 WHERE a.action = 'CUSTOMER_EXPIRED'
		   AND a.sanitized_metadata::text LIKE '%' || v.accounting_id || '%'`); got != 0 {
		t.Error("в метаданных аудита оказался accounting_id")
	}
}

// TestIntegrationExpirySparesRenewedCustomer — expiry перечитывает
// expires_at под locком, поэтому renewal, закоммиченный до захвата строки, не
// отменяется.
func TestIntegrationExpirySparesRenewedCustomer(t *testing.T) {
	stack := newExpiryStack(t)
	seedActiveCustomer(t, stack)
	expireCustomer(t, stack)

	// Renewal обычной командой: expires_at снова в будущем.
	renewed := time.Now().UTC().Add(60 * 24 * time.Hour)
	if err := stack.customer.Execute(context.Background(), command(2, 1<<30, renewed)); err != nil {
		t.Fatalf("renewal: %v", err)
	}

	if got := drainExpiry(t, stack.expiry); got != 0 {
		t.Fatalf("шагов истечения %d, ожидалось 0: продлённого customer выборка не находит", got)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_accesses WHERE desired_state <> 'PRESENT'`); got != 0 {
		t.Errorf("renewal не удержал доступ: %d access не в PRESENT", got)
	}
}
