package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	"github.com/RomanRyabinkin/SpiritVPN/internal/nodeagent"
)

// Интеграционные тесты диспетчера операций. Смысл слоя — в SQL: взятие lease под
// SKIP LOCKED, гейт «одна операция на ноду», повторная проверка desired_version и
// сборка протухших lease.
//
// Настоящий gRPC здесь не поднимается: транспорт закрыт юнит-тестами
// пакета nodeagent, а «потерянный ответ» и «нода недоступна» скриптуются стабом в
// одну строку — с живым сервером их пришлось бы подстраивать таймингом.

const testDispatchOwner = "dispatcher-integration"

// seedOperationCount — сколько операций кладёт в очередь seedDispatchQueue.
// manifestFixture даёт две ноды и одну связь между ними, то есть два FREEDOM и
// один BRIDGE: на входной ноде связи (NL-1) операций оказывается две.
const seedOperationCount = 3

// scriptedAgent отвечает по очереди из заготовленных исходов и запоминает вызовы.
type scriptedAgent struct {
	outcomes []nodeagent.Outcome
	fallback nodeagent.Outcome
	// byNode — исход, зависящий от того, к какой ноде обратились.
	byNode map[string]nodeagent.Outcome

	calls   []nodeagent.Endpoint
	present []nodeagent.User
	absent  []string
	// before вызывается ДО ответа: сюда тесты вешают конкурирующее изменение
	// desired state, которое обязано случиться, пока операция «в полёте».
	before func()
}

func (a *scriptedAgent) next(endpoint nodeagent.Endpoint) nodeagent.Outcome {
	a.calls = append(a.calls, endpoint)
	if a.before != nil {
		a.before()
	}

	// Исход, привязанный к ноде, старше очереди: стаб, отвечающий за несколько
	// нод, обязан различать их по node_id, а очередь задаёт лишь порядок вызовов
	// и о том, кому отвечает, ничего не знает.
	if outcome, ok := a.byNode[endpoint.NodeID]; ok {
		return outcome
	}

	if len(a.outcomes) == 0 {
		return a.fallback
	}

	outcome := a.outcomes[0]
	a.outcomes = a.outcomes[1:]
	return outcome
}

func (a *scriptedAgent) EnsureUserPresent(
	_ context.Context,
	endpoint nodeagent.Endpoint,
	_ string,
	user nodeagent.User,
) nodeagent.Outcome {
	a.present = append(a.present, user)
	return a.next(endpoint)
}

func (a *scriptedAgent) EnsureUserAbsent(
	_ context.Context,
	endpoint nodeagent.Endpoint,
	_ string,
	accountingID string,
) nodeagent.Outcome {
	a.absent = append(a.absent, accountingID)
	return a.next(endpoint)
}

// zeroJitter делает next_attempt_at предсказуемым.
type zeroJitter struct{}

func (zeroJitter) Unit() float64 { return 0 }

// dispatchStack — материализация и диспетчер поверх одного пула и одного шифра.
type dispatchStack struct {
	manifest    *app.ApplyFleetManifest
	customer    *app.ApplyCustomerAccess
	materialize *app.MaterializeManifest
	links       *app.GetCustomerAccessLinks
	dispatch    *app.DispatchOperations
	agent       *scriptedAgent
	pool        *pgxpool.Pool
}

func newDispatchStack(t *testing.T, leaseTTL time.Duration) dispatchStack {
	t.Helper()

	customer, pool := newFixture(t)
	cipher := testCipher(t)
	repo := New(pool)
	agent := &scriptedAgent{fallback: agentApplied()}

	return dispatchStack{
		manifest: app.NewApplyFleetManifest(repo),
		customer: customer,
		materialize: app.NewMaterializeManifest(
			repo, crypto.NewGenerator(), cipher, testWorkerOwner, time.Minute),
		links:    app.NewGetCustomerAccessLinks(repo, cipher),
		dispatch: app.NewDispatchOperations(repo, agent, cipher, zeroJitter{}, testDispatchOwner, leaseTTL),
		agent:    agent,
		pool:     pool,
	}
}

func agentApplied() nodeagent.Outcome {
	return nodeagent.Outcome{Result: domain.AttemptSucceeded, Code: nodeagent.CodeApplied}
}

// seedDispatchQueue заводит топологию и одного customer: в очереди оказываются
// PENDING-операции на оба его access.
func seedDispatchQueue(t *testing.T, stack dispatchStack) {
	t.Helper()

	applyManifest(t, stack.manifest, manifestFixture(7), false)

	expiresAt := time.Now().UTC().Add(30 * 24 * time.Hour)
	if err := stack.customer.Execute(context.Background(), command(1, 1<<30, expiresAt)); err != nil {
		t.Fatalf("ApplyCustomerAccess: %v", err)
	}
	drainMaterialization(t, stack.materialize)
}

// drainDispatch крутит диспетчер до исчерпания очереди и возвращает число шагов.
func drainDispatch(t *testing.T, uc *app.DispatchOperations) int {
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

	t.Fatal("диспетчер не сошёлся за 100 шагов")
	return steps
}

// TestIntegrationDispatchDeliversAndMakesLinkReady — сквозной путь: операция
// доезжает до агента, apply_state становится APPLIED, и ссылка впервые отдаётся
// как READY без ручной правки состояния.
func TestIntegrationDispatchDeliversAndMakesLinkReady(t *testing.T) {
	stack := newDispatchStack(t, time.Minute)
	seedDispatchQueue(t, stack)

	pending := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations WHERE status = 'PENDING'`)
	if pending == 0 {
		t.Fatal("подготовка: очередь операций пуста")
	}

	if got := drainDispatch(t, stack.dispatch); int64(got) != pending {
		t.Fatalf("шагов доставки %d, операций в очереди %d", got, pending)
	}

	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations WHERE status <> 'SUCCEEDED'`); got != 0 {
		t.Errorf("незавершённых операций %d, ожидалось 0", got)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations WHERE completed_at IS NULL`); got != 0 {
		t.Errorf("операций без completed_at %d", got)
	}
	// Lease снимается: ноду больше держать не за что.
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations
		 WHERE lease_owner IS NOT NULL OR lease_expires_at IS NOT NULL`); got != 0 {
		t.Error("lease завершённой операции не снят")
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_accesses WHERE apply_state <> 'APPLIED'`); got != 0 {
		t.Errorf("access не в APPLIED: %d", got)
	}

	// Payload собран из строки access: egress_key связи уехал дословно.
	var bridgeEgress bool
	for _, user := range stack.agent.present {
		if user.EgressKey == "de-exit" {
			bridgeEgress = true
		}
	}
	if !bridgeEgress {
		t.Errorf("egress_tag связи не доехал до агента: %+v", stack.agent.present)
	}

	links, err := stack.links.Execute(context.Background(), testCustomerID)
	if err != nil {
		t.Fatalf("GetCustomerAccessLinks: %v", err)
	}
	if len(links) == 0 {
		t.Fatal("ссылок нет")
	}
	for _, link := range links {
		if link.Status.State != domain.LinkStateReady {
			t.Errorf("ссылка %s в состоянии %s (%s), ожидалось READY",
				link.Kind, link.Status.State, link.Status.Reason)
		}
		if link.URI == "" {
			t.Errorf("READY-ссылка %s без URI", link.Kind)
		}
	}
}

// TestIntegrationDispatchOneInFlightPerNode — одновременно на одну ноду
// уходит не более одной mutating operation. Гейт держится в SQL, а не числом
// горутин.
func TestIntegrationDispatchOneInFlightPerNode(t *testing.T) {
	stack := newDispatchStack(t, time.Minute)
	seedDispatchQueue(t, stack)

	// Обе операции customer сидят на NL-1 и DE-1 по одной; добавим вторую на NL-1,
	// чтобы гейт вообще было чем проверить.
	nodeWithTwo := scalar[string](t, stack.pool,
		`SELECT node_id FROM agent_operations GROUP BY node_id ORDER BY count(*) DESC, node_id LIMIT 1`)

	repo := New(stack.pool)
	first, err := repo.LeaseNext(context.Background(), testDispatchOwner, time.Minute)
	if err != nil {
		t.Fatalf("LeaseNext: %v", err)
	}
	if first == nil {
		t.Fatal("первая операция не взята")
	}

	// Пока первая IN_FLIGHT, вторая операция ТОЙ ЖЕ ноды браться не должна.
	for range 5 {
		next, err := repo.LeaseNext(context.Background(), testDispatchOwner, time.Minute)
		if err != nil {
			t.Fatalf("LeaseNext: %v", err)
		}
		if next == nil {
			break
		}
		if next.Endpoint.NodeID == first.Endpoint.NodeID {
			t.Fatalf("вторая операция взята на занятой ноде %s", next.Endpoint.NodeID)
		}
	}

	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations WHERE status = 'IN_FLIGHT' AND node_id = $1`,
		nodeWithTwo); got > 1 {
		t.Errorf("на ноде %s одновременно %d IN_FLIGHT", nodeWithTwo, got)
	}
}

// TestIntegrationDispatchRefusesSecondInFlightOnNode — инвариант «одна
// операция на ноду» держится структурно, а не только гейтом в запросе.
//
// Гейт читает committed-снимок, поэтому два воркера, взявшие в один момент разные
// операции одной ноды, оба его проходят. Здесь эта гонка воспроизводится точно:
// первая операция уже IN_FLIGHT в НЕЗАКОММИЧЕНной транзакции, и для второй
// попытки её как будто нет.
func TestIntegrationDispatchRefusesSecondInFlightOnNode(t *testing.T) {
	stack := newDispatchStack(t, time.Minute)
	seedDispatchQueue(t, stack)
	ctx := context.Background()

	// Нода с двумя операциями — входная нода связи.
	busy := scalar[string](t, stack.pool,
		`SELECT node_id FROM agent_operations GROUP BY node_id HAVING count(*) > 1`)

	// Операции остальных нод убираются из выборки. Без этого ORDER BY отдаёт
	// глобально первую готовую операцию, она оказывается на свободной ноде, и
	// конкуренции за busy не возникает вовсе — тест проходил бы и без индекса.
	exec(t, stack.pool,
		`UPDATE agent_operations SET next_attempt_at = now() + interval '1 hour' WHERE node_id <> $1`, busy)

	// Транзакция-конкурент: взяла операцию и ещё не закоммитилась.
	rival, err := stack.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = rival.Rollback(ctx) }()

	if _, err := rival.Exec(ctx,
		`UPDATE agent_operations
		 SET status = 'IN_FLIGHT', lease_owner = 'соперник', lease_expires_at = now() + interval '1 minute'
		 WHERE operation_id = (
		     SELECT operation_id FROM agent_operations WHERE node_id = $1 ORDER BY operation_id LIMIT 1
		 )`, busy); err != nil {
		t.Fatalf("подготовка конкурента: %v", err)
	}

	// Пока конкурент не закоммитился, вторая операция той же ноды проходит гейт и
	// упирается в индекс. Ждать придётся его commit — отсюда отдельная горутина.
	leased := make(chan *app.LeasedOperation, 1)
	failed := make(chan error, 1)
	go func() {
		operation, err := New(stack.pool).LeaseNext(ctx, testDispatchOwner, time.Minute)
		if err != nil {
			failed <- err
			return
		}
		leased <- operation
	}()

	// Даём попытке дойти до блокировки на индексе, затем фиксируем конкурента.
	time.Sleep(200 * time.Millisecond)
	if err := rival.Commit(ctx); err != nil {
		t.Fatalf("Commit конкурента: %v", err)
	}

	select {
	case err := <-failed:
		t.Fatalf("проигранная гонка приехала ошибкой, а не пустым результатом: %v", err)
	case operation := <-leased:
		if operation != nil && operation.Endpoint.NodeID == busy {
			t.Fatalf("на занятой ноде %s взята вторая операция", busy)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("LeaseNext не вернулся после commit конкурента")
	}

	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations WHERE status = 'IN_FLIGHT' AND node_id = $1`,
		busy); got != 1 {
		t.Errorf("на ноде %s одновременно %d IN_FLIGHT, ожидалась 1", busy, got)
	}
}

// TestIntegrationDispatchRetriesUnavailableAgent — UNAVAILABLE ретраится с
// backoff, а не гасит access навсегда.
func TestIntegrationDispatchRetriesUnavailableAgent(t *testing.T) {
	stack := newDispatchStack(t, time.Minute)
	seedDispatchQueue(t, stack)

	stack.agent.fallback = nodeagent.Outcome{
		Result:  domain.AttemptRetryable,
		Code:    nodeagent.CodeUnavailable,
		Message: "нода недоступна",
	}

	if _, err := stack.dispatch.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations WHERE status = 'RETRY_WAIT'`); got != 1 {
		t.Fatalf("операций в RETRY_WAIT %d, ожидалась 1", got)
	}
	// attempt_count увеличен взятием lease, jitter нулевой: первая
	// пауза — нижняя половина от двух секунд.
	if got := scalar[int32](t, stack.pool,
		`SELECT attempt_count FROM agent_operations WHERE status = 'RETRY_WAIT'`); got != 1 {
		t.Errorf("attempt_count %d, ожидался 1", got)
	}
	if got := scalar[bool](t, stack.pool,
		`SELECT next_attempt_at > now() FROM agent_operations WHERE status = 'RETRY_WAIT'`); !got {
		t.Error("next_attempt_at не в будущем: ретрай уйдёт немедленно и захлестнёт ноду")
	}
	if got := scalar[string](t, stack.pool,
		`SELECT last_error_code FROM agent_operations WHERE status = 'RETRY_WAIT'`); got != nodeagent.CodeUnavailable {
		t.Errorf("last_error_code %q, ожидался %q", got, nodeagent.CodeUnavailable)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_accesses WHERE apply_state = 'RETRYING'`); got != 1 {
		t.Errorf("access в RETRYING %d, ожидался 1", got)
	}

	// Остальные операции очереди тоже упираются в недоступную ноду и уходят ждать.
	if got := drainDispatch(t, stack.dispatch); got != seedOperationCount-1 {
		t.Fatalf("шагов %d, ожидалось %d", got, seedOperationCount-1)
	}

	// Главное: ни одна не повторяется до истечения паузы. Иначе недоступная нода
	// получала бы залп на полной скорости цикла.
	if got := drainDispatch(t, stack.dispatch); got != 0 {
		t.Errorf("шагов на повторном проходе %d, ожидалось 0: ретрай не ждёт next_attempt_at", got)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations WHERE status = 'RETRY_WAIT' AND attempt_count = 1`); got != seedOperationCount {
		t.Errorf("операций с одной попыткой %d, ожидалось %d", got, seedOperationCount)
	}
}

// TestIntegrationDispatchPermanentIsTerminal — permanent остаётся в terminal
// failed state и не повторяется.
func TestIntegrationDispatchPermanentIsTerminal(t *testing.T) {
	stack := newDispatchStack(t, time.Minute)
	seedDispatchQueue(t, stack)

	stack.agent.fallback = nodeagent.Outcome{
		Result:  domain.AttemptPermanent,
		Code:    nodeagent.CodeInvalidArgument,
		Message: "агент не принял payload",
	}

	drainDispatch(t, stack.dispatch)

	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations WHERE status <> 'FAILED_PERMANENT'`); got != 0 {
		t.Errorf("операций не в FAILED_PERMANENT %d", got)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations WHERE next_attempt_at IS NOT NULL`); got != 0 {
		t.Error("permanent-операция запланирована на повтор")
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_accesses WHERE apply_state <> 'FAILED'`); got != 0 {
		t.Errorf("access не в FAILED: %d", got)
	}

	// Второй проход не находит работы: permanent не хот-лупится.
	if got := drainDispatch(t, stack.dispatch); got != 0 {
		t.Errorf("шагов на повторном проходе %d, ожидалось 0", got)
	}
}

// TestIntegrationDispatchLostResponseIsSafe — потеря ответа после применения
// безопасна, потому что повтор той же операции идемпотентен.
func TestIntegrationDispatchLostResponseIsSafe(t *testing.T) {
	stack := newDispatchStack(t, time.Minute)
	seedDispatchQueue(t, stack)

	// Агент применил изменение, но ответ не доехал: backend видит DEADLINE_EXCEEDED.
	stack.agent.outcomes = []nodeagent.Outcome{{
		Result:  domain.AttemptRetryable,
		Code:    nodeagent.CodeDeadline,
		Message: "ответ не получен",
	}}

	if _, err := stack.dispatch.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	operationID := scalar[uuid.UUID](t, stack.pool,
		`SELECT operation_id FROM agent_operations WHERE status = 'RETRY_WAIT'`)

	// Пауза прошла. Повтор идёт ТОЙ ЖЕ операцией: operation_id стабилен между
	// попытками, и агент опознаёт повтор по нему.
	exec(t, stack.pool,
		`UPDATE agent_operations SET next_attempt_at = now() WHERE operation_id = $1`, operationID)

	// На повторе агент отвечает ALREADY_APPLIED — состояние уже точное.
	stack.agent.fallback = nodeagent.Outcome{
		Result: domain.AttemptSucceeded,
		Code:   nodeagent.CodeAlreadyApplied,
	}
	drainDispatch(t, stack.dispatch)

	if got := scalar[string](t, stack.pool,
		`SELECT status FROM agent_operations WHERE operation_id = $1`, operationID); got != "SUCCEEDED" {
		t.Errorf("статус повторённой операции %q, ожидался SUCCEEDED", got)
	}
	if got := scalar[int32](t, stack.pool,
		`SELECT attempt_count FROM agent_operations WHERE operation_id = $1`, operationID); got != 2 {
		t.Errorf("attempt_count %d, ожидался 2", got)
	}
	// Дублирующей операции не завелось: повтор — это та же строка outbox.
	if got := scalar[int64](t, stack.pool, `SELECT count(*) FROM agent_operations`); got != seedOperationCount {
		t.Errorf("строк в outbox %d, ожидалось %d — повтор не создаёт новых", got, seedOperationCount)
	}
}

// TestIntegrationDispatchStaleResultDoesNotOverwrite — результат операции,
// чья desired_version устарела ПОКА ШЁЛ RPC, не меняет apply_state актуальной
// версии, а сама операция становится SUPERSEDED.
func TestIntegrationDispatchStaleResultDoesNotOverwrite(t *testing.T) {
	stack := newDispatchStack(t, time.Minute)
	seedDispatchQueue(t, stack)

	target := scalar[uuid.UUID](t, stack.pool,
		`SELECT access_id FROM agent_operations ORDER BY operation_id LIMIT 1`)

	// Пока операция «в полёте», desired state меняется: так делает материализация
	// или renewal. Строка access уходит на версию вперёд и в PENDING.
	stack.agent.before = func() {
		exec(t, stack.pool,
			`UPDATE vpn_accesses
			 SET desired_version = desired_version + 1, apply_state = 'PENDING'
			 WHERE access_id = $1`, target)
	}

	if _, err := stack.dispatch.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	stack.agent.before = nil

	staleStatus := scalar[string](t, stack.pool,
		`SELECT status FROM agent_operations WHERE access_id = $1`, target)
	if staleStatus != "SUCCEEDED" {
		t.Errorf("статус %q, ожидался SUCCEEDED: операция действительно доехала", staleStatus)
	}
	// Главное: apply_state актуальной версии не переписан успехом старой.
	if got := scalar[string](t, stack.pool,
		`SELECT apply_state FROM vpn_accesses WHERE access_id = $1`, target); got != "PENDING" {
		t.Errorf("apply_state %q, ожидался PENDING: результат устаревшей операции его переписал", got)
	}
}

// TestIntegrationDispatchSkipsSupersededBeforeRPC — устаревшая операция не
// уезжает агенту вовсе и снимается с очереди как SUPERSEDED.
func TestIntegrationDispatchSkipsSupersededBeforeRPC(t *testing.T) {
	stack := newDispatchStack(t, time.Minute)
	seedDispatchQueue(t, stack)

	target := scalar[uuid.UUID](t, stack.pool,
		`SELECT access_id FROM agent_operations ORDER BY operation_id LIMIT 1`)
	// Версия ушла вперёд ДО того, как диспетчер взял операцию.
	exec(t, stack.pool,
		`UPDATE vpn_accesses SET desired_version = desired_version + 1 WHERE access_id = $1`, target)

	drainDispatch(t, stack.dispatch)

	if got := scalar[string](t, stack.pool,
		`SELECT status FROM agent_operations WHERE access_id = $1`, target); got != "SUPERSEDED" {
		t.Errorf("статус %q, ожидался SUPERSEDED", got)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations
		 WHERE access_id = $1 AND (next_attempt_at IS NOT NULL OR completed_at IS NULL)`, target); got != 0 {
		t.Error("SUPERSEDED не терминален: осталась запланированная попытка")
	}

	// Агент получил команды по всем access, КРОМЕ устаревшего.
	if got := len(stack.agent.calls); got != seedOperationCount-1 {
		t.Errorf("вызовов агента %d, ожидалось %d: устаревшая операция не должна уезжать",
			got, seedOperationCount-1)
	}
}

// TestIntegrationDispatchReapsExpiredLease — после истечения lease устаревшая
// операция становится SUPERSEDED, актуальная возвращается в очередь.
func TestIntegrationDispatchReapsExpiredLease(t *testing.T) {
	stack := newDispatchStack(t, time.Minute)
	seedDispatchQueue(t, stack)

	repo := New(stack.pool)

	// Два воркера взяли по операции и умерли, не записав результата.
	for range 2 {
		if _, err := repo.LeaseNext(context.Background(), "умерший-воркер", time.Minute); err != nil {
			t.Fatalf("LeaseNext: %v", err)
		}
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations WHERE status = 'IN_FLIGHT'`); got != 2 {
		t.Fatalf("подготовка: IN_FLIGHT %d, ожидалось 2", got)
	}

	// У одной из них desired state успел смениться, у другой нет.
	superseded := scalar[uuid.UUID](t, stack.pool,
		`SELECT access_id FROM agent_operations ORDER BY operation_id LIMIT 1`)
	exec(t, stack.pool,
		`UPDATE vpn_accesses SET desired_version = desired_version + 1 WHERE access_id = $1`, superseded)

	// Lease протухли.
	exec(t, stack.pool, `UPDATE agent_operations SET lease_expires_at = now() - interval '1 second'`)

	reaped, err := repo.ReapExpiredLeases(context.Background(), 100)
	if err != nil {
		t.Fatalf("ReapExpiredLeases: %v", err)
	}
	if reaped != 2 {
		t.Fatalf("собрано %d lease, ожидалось 2", reaped)
	}

	if got := scalar[string](t, stack.pool,
		`SELECT status FROM agent_operations WHERE access_id = $1`, superseded); got != "SUPERSEDED" {
		t.Errorf("статус устаревшей операции %q, ожидался SUPERSEDED", got)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations WHERE status = 'RETRY_WAIT'`); got != 1 {
		t.Errorf("операций, вернувшихся в очередь, %d, ожидалась 1", got)
	}
	// Ни одна не держит ноду: гейт снят вместе с lease.
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations WHERE lease_owner IS NOT NULL`); got != 0 {
		t.Error("собранная операция сохранила lease_owner")
	}
	// attempt_count не откатывается: попытка действительно состоялась.
	if got := scalar[int32](t, stack.pool,
		`SELECT attempt_count FROM agent_operations WHERE status = 'RETRY_WAIT'`); got != 1 {
		t.Errorf("attempt_count %d, ожидался 1", got)
	}

	// Возвращённая в очередь операция забирается следующим же шагом.
	if got := drainDispatch(t, stack.dispatch); got == 0 {
		t.Error("операция после сбора lease не подхвачена")
	}
}

// TestIntegrationDispatchLoadsEndpointFromManifest — payload собирается из
// АКТУАЛЬНОЙ строки ноды, а испорченный agent_config даёт нулевой endpoint,
// который отвергнет уже клиент агента.
//
// Классификацию этого исхода проверяют юнит-тесты nodeagent: здесь важно ровно то,
// за что отвечает SQL-слой, — что endpoint читается из колонки и что битый jsonb не
// роняет шаг.
func TestIntegrationDispatchLoadsEndpointFromManifest(t *testing.T) {
	stack := newDispatchStack(t, time.Minute)
	seedDispatchQueue(t, stack)

	repo := New(stack.pool)

	operation, err := repo.LeaseNext(context.Background(), testDispatchOwner, time.Minute)
	if err != nil {
		t.Fatalf("LeaseNext: %v", err)
	}
	if operation == nil {
		t.Fatal("операция не взята")
	}

	nodeID := operation.Endpoint.NodeID
	want := nodeagent.Endpoint{
		NodeID:              nodeID,
		Address:             "10.0.0.11:9443",
		TLSServerName:       nodeID + ".agent.internal",
		CertificateIdentity: "spiffe://spiritvpn/node/" + nodeID,
	}
	if operation.Endpoint != want {
		t.Errorf("endpoint %+v, ожидался %+v", operation.Endpoint, want)
	}
	if operation.Flow != domain.FlowXTLSRprxVision {
		t.Errorf("flow %q, ожидался %q", operation.Flow, domain.FlowXTLSRprxVision)
	}

	// Испорченная колонка не роняет шаг: endpoint приезжает нулевым, и вызов
	// отвергнет клиент — retryable с alert, а не паника и не вечный FAILED.
	exec(t, stack.pool, `UPDATE vpn_nodes SET agent_config = '{}'::jsonb`)

	next, err := repo.LeaseNext(context.Background(), testDispatchOwner, time.Minute)
	if err != nil {
		t.Fatalf("LeaseNext после порчи agent_config: %v", err)
	}
	if next == nil {
		t.Fatal("вторая операция не взята")
	}
	if next.Endpoint.Address != "" || next.Endpoint.CertificateIdentity != "" {
		t.Errorf("из пустого agent_config собрался endpoint %+v", next.Endpoint)
	}
	if next.Endpoint.NodeID == "" {
		t.Error("node_id потерян: без него не понять, чья нода сломана")
	}
}

// TestIntegrationDispatchAbsentCarriesNoCredential — удаление матчится по
// accounting_id, и credential для него из базы даже не читается.
func TestIntegrationDispatchAbsentCarriesNoCredential(t *testing.T) {
	stack := newDispatchStack(t, time.Minute)
	seedDispatchQueue(t, stack)

	// Доставляем всё, что есть, и убираем связь: появится ENSURE_ABSENT.
	drainDispatch(t, stack.dispatch)

	shrunk := manifestFixture(8)
	shrunk.Fleets[0].Bridges = nil
	applyManifest(t, stack.manifest, shrunk, true)
	drainMaterialization(t, stack.materialize)

	operation, err := New(stack.pool).LeaseNext(context.Background(), testDispatchOwner, time.Minute)
	if err != nil {
		t.Fatalf("LeaseNext: %v", err)
	}
	if operation == nil {
		t.Fatal("операция удаления не взята")
	}
	if operation.DesiredState != domain.DesiredStateAbsent {
		t.Fatalf("desired_state %q, ожидался ABSENT", operation.DesiredState)
	}
	if len(operation.Credential.Blob) != 0 || operation.Credential.KeyID != "" {
		t.Error("для ABSENT из базы прочитан credential")
	}
	if operation.AccountingID == "" {
		t.Error("accounting_id пуст: удалять нечего")
	}
}

// TestIntegrationPartialFleetReadiness — fleet, где часть нод отвечает, а
// часть нет.
//
// Стек берётся usage'овый: он единственный собирает манифест, материализацию,
// доставку и ссылки поверх одного пула, а проверяется здесь именно то, что
// видит customer после частично удавшейся доставки.
//
// Утверждение: недоступность ноды не меняет ни desired state, ни
// состав fleet, а готовые ссылки отдаются, не дожидаясь неготовых. Обратное —
// «пока не доставили всем, не показываем никому» — превратило бы одну упавшую
// ноду в полную потерю сервиса для всех customer этого fleet.
func TestIntegrationPartialFleetReadiness(t *testing.T) {
	stack := newUsageStack(t)

	// Своя раскладка вместо seedUsageCustomer: тому нужна удавшаяся доставка, а
	// здесь она обязана удаться наполовину.
	applyManifest(t, stack.manifest, manifestFixture(7), false)
	expiresAt := time.Now().UTC().Add(30 * 24 * time.Hour)
	if err := stack.customer.Execute(context.Background(), command(1, 1<<30, expiresAt)); err != nil {
		t.Fatalf("ApplyCustomerAccess: %v", err)
	}
	drainMaterialization(t, stack.materialize)

	// DE-1 отвечает, NL-1 недоступна.
	stack.dispatched.byNode = map[string]nodeagent.Outcome{
		"NL-1": {Result: domain.AttemptRetryable, Code: nodeagent.CodeUnavailable},
		"DE-1": agentApplied(),
	}
	drainDispatch(t, stack.dispatch)

	links, err := stack.links.Execute(context.Background(), testCustomerID)
	if err != nil {
		t.Fatalf("GetCustomerAccessLinks: %v", err)
	}
	if len(links) != 3 {
		t.Fatalf("ссылок %d, ожидалось 3", len(links))
	}

	var ready, pending int
	for _, link := range links {
		switch link.Status.State {
		case domain.LinkStateReady:
			ready++
			if link.URI == "" {
				t.Errorf("READY-ссылка %s без URI", link.Kind)
			}
		case domain.LinkStatePending:
			pending++
			// У недоставленной ссылки URI нет и быть не может — она бы не
			// работала, а customer счёл бы её рабочей.
			if link.URI != "" {
				t.Errorf("PENDING-ссылка %s отдана с URI", link.Kind)
			}
		default:
			t.Errorf("ссылка %s в состоянии %s", link.Kind, link.Status.State)
		}
	}
	// На NL-1 два access (FREEDOM и BRIDGE, где она входная), на DE-1 один.
	if ready != 1 || pending != 2 {
		t.Errorf("READY %d, PENDING %d; ожидалось 1 и 2", ready, pending)
	}

	// Недоступность ноды не трогает desired state. Иначе повторная попытка
	// доставила бы не то, что задумано, а то, во что состояние выродилось.
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_accesses WHERE desired_state = 'PRESENT' AND retired_at IS NULL`); got != 3 {
		t.Errorf("access в PRESENT %d, ожидалось 3", got)
	}
	// Работа не потеряна: операции недоступной ноды ждут следующей попытки в
	// RETRY_WAIT, а не осели в IN_FLIGHT навсегда.
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations
		 WHERE node_id = 'NL-1' AND status = 'RETRY_WAIT' AND next_attempt_at > now()`); got != 2 {
		t.Errorf("ждущих повтора операций на NL-1 %d, ожидалось 2", got)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations WHERE status = 'IN_FLIGHT'`); got != 0 {
		t.Errorf("зависших IN_FLIGHT %d, ожидалось 0", got)
	}

	// Восстановившаяся нода догоняет сама, без внешнего вмешательства.
	stack.dispatched.byNode["NL-1"] = agentApplied()
	exec(t, stack.pool, `UPDATE agent_operations SET next_attempt_at = now() WHERE node_id = 'NL-1'`)
	drainDispatch(t, stack.dispatch)

	links, err = stack.links.Execute(context.Background(), testCustomerID)
	if err != nil {
		t.Fatalf("GetCustomerAccessLinks после восстановления: %v", err)
	}
	for _, link := range links {
		if link.Status.State != domain.LinkStateReady {
			t.Errorf("ссылка %s в состоянии %s, ожидалось READY", link.Kind, link.Status.State)
		}
	}
}
