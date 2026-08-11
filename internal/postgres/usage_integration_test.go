package postgres

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	"github.com/RomanRyabinkin/SpiritVPN/internal/nodeagent"
)

// Интеграционные тесты учёта трафика. Смысл слоя в SQL: дедуп через
// ON CONFLICT DO NOTHING RETURNING, выбор периода по collected_at, row lock
// node_quota_usage и то, что порог активируется один раз.

const testUsageOwner = "usage-integration"

// spoolAgent отдаёт заготовленные batch'и и запоминает подтверждения.
//
// Спул у каждой ноды свой: общий на всех означал бы, что DE-1 отчитывается за
// accounting_id, живущий на NL-1, — а это чужая нода и карантин, то есть тест
// проверял бы совсем не то, что задумано.
type spoolAgent struct {
	batches map[string][]nodeagent.UsageBatch
	// failWith непусто — опрос проваливается.
	failWith string

	acked map[string][]nodeagent.UsageCursor
}

func (a *spoolAgent) set(nodeID string, batches ...nodeagent.UsageBatch) {
	if a.batches == nil {
		a.batches = make(map[string][]nodeagent.UsageBatch)
	}
	a.batches[nodeID] = batches
}

func (a *spoolAgent) GetNodeState(
	_ context.Context,
	endpoint nodeagent.Endpoint,
	acknowledged nodeagent.UsageCursor,
	_ uint32,
) nodeagent.PullOutcome {
	if a.acked == nil {
		a.acked = make(map[string][]nodeagent.UsageCursor)
	}
	a.acked[endpoint.NodeID] = append(a.acked[endpoint.NodeID], acknowledged)

	if a.failWith != "" {
		return nodeagent.PullOutcome{Code: a.failWith, Message: "нода недоступна"}
	}

	return nodeagent.PullOutcome{
		State: &nodeagent.NodeState{
			NodeID:        endpoint.NodeID,
			Batches:       a.batches[endpoint.NodeID],
			XrayReachable: true,
		},
		Code: nodeagent.CodeApplied,
	}
}

// usageStack — учёт вместе с соседями: доступ надо завести, а блокировку по квоте
// довести до агента.
type usageStack struct {
	manifest    *app.ApplyFleetManifest
	customer    *app.ApplyCustomerAccess
	materialize *app.MaterializeManifest
	usage       *app.PullUsage
	dispatch    *app.DispatchOperations
	links       *app.GetCustomerAccessLinks
	agent       *spoolAgent
	dispatched  *scriptedAgent
	pool        *pgxpool.Pool
}

func newUsageStack(t *testing.T) usageStack {
	t.Helper()

	customer, pool := newFixture(t)
	cipher := testCipher(t)
	repo := New(pool)
	agent := &spoolAgent{}
	dispatched := &scriptedAgent{fallback: agentApplied()}

	return usageStack{
		manifest: app.NewApplyFleetManifest(repo),
		customer: customer,
		materialize: app.NewMaterializeManifest(
			repo, crypto.NewGenerator(), cipher, testWorkerOwner, time.Minute),
		usage: app.NewPullUsage(repo, agent, crypto.NewGenerator(), testLogger(t),
			testUsageOwner, time.Minute, 0),
		dispatch:   app.NewDispatchOperations(repo, dispatched, cipher, zeroJitter{}, testDispatchOwner, time.Minute),
		links:      app.NewGetCustomerAccessLinks(repo, cipher),
		agent:      agent,
		dispatched: dispatched,
		pool:       pool,
	}
}

// testLogger гасит вывод: воркер логирует недоступность нод и смену спула, и в
// выводе тестов это только шум.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) { return len(p), nil }

// seedUsageCustomer заводит топологию, customer с заданной квотой и доставляет
// доступ. Возвращает accounting_id FREEDOM-access на NL-1.
func seedUsageCustomer(t *testing.T, stack usageStack, quotaBytes uint64) string {
	t.Helper()

	applyManifest(t, stack.manifest, manifestFixture(7), false)

	expiresAt := time.Now().UTC().Add(30 * 24 * time.Hour)
	if err := stack.customer.Execute(context.Background(), command(1, quotaBytes, expiresAt)); err != nil {
		t.Fatalf("ApplyCustomerAccess: %v", err)
	}
	drainMaterialization(t, stack.materialize)
	drainDispatch(t, stack.dispatch)

	return scalar[string](t, stack.pool,
		`SELECT accounting_id FROM vpn_accesses
		 WHERE entry_node_id = 'NL-1' AND kind = 'FREEDOM'`)
}

// batchOf собирает batch одной ноды.
func batchOf(sequence uint64, collectedAt time.Time, items ...nodeagent.UserUsage) nodeagent.UsageBatch {
	return nodeagent.UsageBatch{
		Cursor:      nodeagent.UsageCursor{SpoolID: "spool-1", Sequence: sequence},
		CollectedAt: collectedAt,
		Items:       items,
	}
}

// testFleetNodes — сколько нод в manifestFixture: NL-1 и DE-1.
const testFleetNodes = 2

// pullRound опрашивает каждую ноду ровно один раз.
//
// Не «пока есть прогресс»: интервал опроса в тестах нулевой, поэтому нода
// становится готова к следующему опросу немедленно, и такой цикл не сошёлся бы
// никогда. В бою интервал ненулевой, и цикл воркера засыпает сам.
//
// Ноды берутся в порядке updated_at, поэтому за testFleetNodes шагов каждая
// опрашивается ровно однажды.
func pullRound(t *testing.T, uc *app.PullUsage) {
	t.Helper()

	for step := range testFleetNodes {
		progressed, err := uc.ProcessNext(context.Background())
		if err != nil {
			t.Fatalf("ProcessNext на шаге %d: %v", step, err)
		}
		if !progressed {
			t.Fatalf("на шаге %d опрашивать нечего, а нод %d", step, testFleetNodes)
		}
	}
}

func nodeTotal(t *testing.T, stack usageStack, nodeID string) uint64 {
	t.Helper()
	return uint64(scalar[int64](t, stack.pool,
		`SELECT total_bytes::bigint FROM node_quota_usage u
		 JOIN quota_periods p ON p.quota_period_id = u.quota_period_id
		 WHERE u.node_id = $1 AND p.closed_at IS NULL`, nodeID))
}

// TestIntegrationUsageAccruesDeltas — §12: дельты складываются в counters ноды.
func TestIntegrationUsageAccruesDeltas(t *testing.T) {
	stack := newUsageStack(t)
	accountingID := seedUsageCustomer(t, stack, 1<<30)

	now := time.Now().UTC()
	stack.agent.set("NL-1",
		batchOf(1, now, nodeagent.UserUsage{AccountingID: accountingID, UplinkBytes: 100, DownlinkBytes: 200}),
	)

	pullRound(t, stack.usage)

	if got := nodeTotal(t, stack, "NL-1"); got != 300 {
		t.Errorf("расход NL-1 %d, ожидалось 300", got)
	}
	// Курсор подтверждён только после commit (решение 63).
	if got := scalar[int64](t, stack.pool,
		`SELECT acked_sequence::bigint FROM node_usage_cursors WHERE node_id = 'NL-1'`); got != 1 {
		t.Errorf("acked_sequence %d, ожидалось 1", got)
	}
	if got := scalar[string](t, stack.pool,
		`SELECT spool_id FROM node_usage_cursors WHERE node_id = 'NL-1'`); got != "spool-1" {
		t.Errorf("spool_id %q, ожидался spool-1", got)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM traffic_usage_items_processed WHERE result = 'APPLIED'`); got != 1 {
		t.Errorf("зарегистрировано items %d, ожидался 1", got)
	}
}

// TestIntegrationUsageDeduplicatesRepeatedBatch — §12: повторный pull, перезапуск
// воркера и повтор batch не удваивают totals.
func TestIntegrationUsageDeduplicatesRepeatedBatch(t *testing.T) {
	stack := newUsageStack(t)
	accountingID := seedUsageCustomer(t, stack, 1<<30)

	now := time.Now().UTC()
	stack.agent.set("NL-1",
		batchOf(1, now, nodeagent.UserUsage{AccountingID: accountingID, UplinkBytes: 100, DownlinkBytes: 200}),
	)
	pullRound(t, stack.usage)

	// Агент прислал тот же batch снова: подтверждение потерялось.
	// Курсор отматывается назад, чтобы монотонность не отсекла batch раньше дедупа
	// и проверялся именно ON CONFLICT DO NOTHING.
	exec(t, stack.pool, `UPDATE node_usage_cursors SET acked_sequence = 0 WHERE node_id = 'NL-1'`)
	pullRound(t, stack.usage)

	if got := nodeTotal(t, stack, "NL-1"); got != 300 {
		t.Errorf("расход NL-1 %d, ожидалось 300 — повтор batch начислен дважды", got)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM traffic_usage_items_processed`); got != 1 {
		t.Errorf("строк реестра %d, ожидалась 1", got)
	}
}

// TestIntegrationUsageSkipsAcknowledgedBatch — §12, шаг 1: уже подтверждённый
// batch является идемпотентным no-op и не открывает транзакций.
func TestIntegrationUsageSkipsAcknowledgedBatch(t *testing.T) {
	stack := newUsageStack(t)
	accountingID := seedUsageCustomer(t, stack, 1<<30)

	now := time.Now().UTC()
	stack.agent.set("NL-1",
		batchOf(1, now, nodeagent.UserUsage{AccountingID: accountingID, UplinkBytes: 100, DownlinkBytes: 200}),
	)
	pullRound(t, stack.usage)
	pullRound(t, stack.usage)

	if got := nodeTotal(t, stack, "NL-1"); got != 300 {
		t.Errorf("расход NL-1 %d, ожидалось 300", got)
	}
}

// TestIntegrationUsageExhaustsQuotaAndBlocksNode — §12, шаг 5: пересечение порога
// гасит ВСЕ access customer на ноде, а §9 доставляет удаления агенту.
func TestIntegrationUsageExhaustsQuotaAndBlocksNode(t *testing.T) {
	stack := newUsageStack(t)
	accountingID := seedUsageCustomer(t, stack, 1000)

	now := time.Now().UTC()
	stack.agent.set("NL-1",
		batchOf(1, now, nodeagent.UserUsage{AccountingID: accountingID, UplinkBytes: 600, DownlinkBytes: 400}),
	)
	pullRound(t, stack.usage)

	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM node_quota_usage WHERE node_id = 'NL-1' AND exhausted_at IS NOT NULL`); got != 1 {
		t.Fatal("отметка исчерпания не поставлена")
	}
	// На NL-1 у customer два access: FREEDOM и BRIDGE, где она входная (§4).
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_accesses WHERE entry_node_id = 'NL-1' AND desired_state = 'ABSENT'`); got != 2 {
		t.Errorf("погашено access на NL-1 %d, ожидалось 2", got)
	}
	// §4: превышение на одной ноде не влияет на access на других.
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_accesses WHERE entry_node_id = 'DE-1' AND desired_state = 'PRESENT'`); got != 1 {
		t.Errorf("квота NL-1 задела DE-1: PRESENT на DE-1 %d, ожидался 1", got)
	}

	drainDispatch(t, stack.dispatch)
	if got := len(stack.dispatched.absent); got != 2 {
		t.Errorf("агент получил %d удалений, ожидалось 2", got)
	}

	links, err := stack.links.Execute(context.Background(), testCustomerID)
	if err != nil {
		t.Fatalf("GetCustomerAccessLinks: %v", err)
	}
	var blocked int
	for _, link := range links {
		if link.Status.Reason == domain.BlockReasonTrafficQuotaExhausted {
			blocked++
		}
	}
	if blocked != 2 {
		t.Errorf("ссылок с TRAFFIC_QUOTA_EXHAUSTED %d, ожидалось 2", blocked)
	}
}

// TestIntegrationUsageThresholdFiresOnce — §12: порог активируется один раз, и
// вторая пачка не плодит повторных Remove operations.
func TestIntegrationUsageThresholdFiresOnce(t *testing.T) {
	stack := newUsageStack(t)
	accountingID := seedUsageCustomer(t, stack, 1000)

	now := time.Now().UTC()
	stack.agent.set("NL-1",
		batchOf(1, now, nodeagent.UserUsage{AccountingID: accountingID, UplinkBytes: 600, DownlinkBytes: 400}),
	)
	pullRound(t, stack.usage)
	operationsAfterFirst := scalar[int64](t, stack.pool, `SELECT count(*) FROM agent_operations`)
	exhaustedAtFirst := scalar[time.Time](t, stack.pool,
		`SELECT exhausted_at FROM node_quota_usage u
		 JOIN quota_periods p ON p.quota_period_id = u.quota_period_id
		 WHERE u.node_id = 'NL-1' AND p.closed_at IS NULL`)

	stack.agent.set("NL-1",
		batchOf(2, now, nodeagent.UserUsage{AccountingID: accountingID, UplinkBytes: 500, DownlinkBytes: 500}),
	)
	pullRound(t, stack.usage)

	// Начисление продолжается после блокировки (§12).
	if got := nodeTotal(t, stack, "NL-1"); got != 2000 {
		t.Errorf("расход %d, ожидалось 2000", got)
	}
	if got := scalar[int64](t, stack.pool, `SELECT count(*) FROM agent_operations`); got != operationsAfterFirst {
		t.Errorf("операций стало %d, было %d — порог сработал повторно", got, operationsAfterFirst)
	}

	// Отметка исчерпания принадлежит моменту ПЕРВОГО пересечения и не должна
	// переставляться каждой следующей пачкой: по ней видно, когда нода встала.
	if got := scalar[time.Time](t, stack.pool,
		`SELECT exhausted_at FROM node_quota_usage u
		 JOIN quota_periods p ON p.quota_period_id = u.quota_period_id
		 WHERE u.node_id = 'NL-1' AND p.closed_at IS NULL`); !got.Equal(exhaustedAtFirst) {
		t.Errorf("exhausted_at переставлен: было %v, стало %v", exhaustedAtFirst, got)
	}
}

// TestIntegrationUsageClosedPeriodIgnored — §11.1: batch закрытого периода
// регистрируется как IGNORED_CLOSED_PERIOD, не меняет historical totals и не
// блокирует access нового периода.
func TestIntegrationUsageClosedPeriodIgnored(t *testing.T) {
	stack := newUsageStack(t)
	accountingID := seedUsageCustomer(t, stack, 1000)

	collectedAt := time.Now().UTC()
	closedPeriod := scalar[string](t, stack.pool,
		`SELECT quota_period_id::text FROM quota_periods WHERE closed_at IS NULL`)

	// Renewal закрывает период и открывает новый (§5, правило 8).
	renewed := time.Now().UTC().Add(60 * 24 * time.Hour)
	if err := stack.customer.Execute(context.Background(), command(2, 1000, renewed)); err != nil {
		t.Fatalf("renewal: %v", err)
	}

	// Batch собран ДО закрытия, значит попадает в уже закрытый период.
	stack.agent.set("NL-1",
		batchOf(1, collectedAt, nodeagent.UserUsage{AccountingID: accountingID, UplinkBytes: 900, DownlinkBytes: 900}),
	)
	pullRound(t, stack.usage)

	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM traffic_usage_items_processed WHERE result = 'IGNORED_CLOSED_PERIOD'`); got != 1 {
		t.Fatalf("items с IGNORED_CLOSED_PERIOD %d, ожидался 1", got)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT coalesce(sum(total_bytes), 0)::bigint FROM node_quota_usage WHERE quota_period_id = $1::uuid`,
		closedPeriod); got != 0 {
		t.Errorf("historical totals закрытого периода изменились: %d", got)
	}
	// Access нового периода не блокируются.
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_accesses WHERE desired_state <> 'PRESENT'`); got != 0 {
		t.Errorf("закрытый период заблокировал %d access", got)
	}
	// Курсор всё равно двигается: batch обработан, и держать его нельзя.
	if got := scalar[int64](t, stack.pool,
		`SELECT acked_sequence::bigint FROM node_usage_cursors WHERE node_id = 'NL-1'`); got != 1 {
		t.Errorf("acked_sequence %d, ожидалось 1", got)
	}
}

// TestIntegrationUsageQuarantinesUnknownAccounting — §12, шаг 6: неизвестный
// accounting_id уходит в карантин и не блокирует остальной batch.
func TestIntegrationUsageQuarantinesUnknownAccounting(t *testing.T) {
	stack := newUsageStack(t)
	accountingID := seedUsageCustomer(t, stack, 1<<30)

	now := time.Now().UTC()
	stack.agent.set("NL-1",
		batchOf(1, now,
			nodeagent.UserUsage{AccountingID: "чужой-id", UplinkBytes: 1, DownlinkBytes: 2},
			nodeagent.UserUsage{AccountingID: accountingID, UplinkBytes: 100, DownlinkBytes: 200},
		),
	)
	pullRound(t, stack.usage)

	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM traffic_batch_quarantine WHERE reason = 'UNKNOWN_ACCOUNTING_ID'`); got != 1 {
		t.Fatalf("карантинных записей %d, ожидалась 1", got)
	}
	// Карантинный item отмечен обработанным, иначе он приезжал бы вечно.
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM traffic_usage_items_processed WHERE result = 'QUARANTINED'`); got != 1 {
		t.Errorf("QUARANTINED в реестре %d, ожидался 1", got)
	}
	// Остальной batch начислен.
	if got := nodeTotal(t, stack, "NL-1"); got != 300 {
		t.Errorf("расход %d, ожидалось 300 — плохой item заблокировал batch", got)
	}
	// В карантине только счётчики байтов, никаких секретов (§12).
	if got := scalar[string](t, stack.pool,
		`SELECT sanitized_payload::text FROM traffic_batch_quarantine`); got != `{"uplink_bytes": 1, "downlink_bytes": 2}` {
		t.Errorf("payload карантина %q", got)
	}
}

// TestIntegrationUsageQuarantinesItemWithoutPeriod — решение 65: item, которому не
// нашлось периода, уходит в карантин, а не помечается IGNORED_CLOSED_PERIOD:
// период не закрыт, его не существует.
func TestIntegrationUsageQuarantinesItemWithoutPeriod(t *testing.T) {
	stack := newUsageStack(t)
	accountingID := seedUsageCustomer(t, stack, 1<<30)

	// Момент сбора раньше начала единственного периода customer.
	collectedAt := scalar[time.Time](t, stack.pool,
		`SELECT started_at FROM quota_periods WHERE closed_at IS NULL`).Add(-time.Hour)

	stack.agent.set("NL-1",
		batchOf(1, collectedAt, nodeagent.UserUsage{AccountingID: accountingID, UplinkBytes: 5, DownlinkBytes: 5}),
	)
	pullRound(t, stack.usage)

	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM traffic_batch_quarantine WHERE reason = 'NO_QUOTA_PERIOD'`); got != 1 {
		t.Fatalf("карантинных записей %d, ожидалась 1", got)
	}
	if got := nodeTotal(t, stack, "NL-1"); got != 0 {
		t.Errorf("расход %d, ожидался 0: начислять было некуда", got)
	}
}

// TestIntegrationUsageSpoolChangeResetsCursor — §12: смена spool_id трактуется как
// новый спул, backend продолжает с его sequence и старые не начисляет.
func TestIntegrationUsageSpoolChangeResetsCursor(t *testing.T) {
	stack := newUsageStack(t)
	accountingID := seedUsageCustomer(t, stack, 1<<30)

	now := time.Now().UTC()
	stack.agent.set("NL-1",
		batchOf(5, now, nodeagent.UserUsage{AccountingID: accountingID, UplinkBytes: 100, DownlinkBytes: 0}),
	)
	pullRound(t, stack.usage)

	// Спул на ноде пересоздан: новый id, нумерация с единицы. Прежний
	// acked_sequence = 5 не должен отсечь batch номер 1.
	stack.agent.set("NL-1", nodeagent.UsageBatch{
		Cursor:      nodeagent.UsageCursor{SpoolID: "spool-2", Sequence: 1},
		CollectedAt: now,
		Items:       []nodeagent.UserUsage{{AccountingID: accountingID, UplinkBytes: 50, DownlinkBytes: 0}},
	})
	pullRound(t, stack.usage)

	if got := nodeTotal(t, stack, "NL-1"); got != 150 {
		t.Errorf("расход %d, ожидалось 150 — batch нового спула не начислен", got)
	}
	if got := scalar[string](t, stack.pool,
		`SELECT spool_id FROM node_usage_cursors WHERE node_id = 'NL-1'`); got != "spool-2" {
		t.Errorf("spool_id %q, ожидался spool-2", got)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT acked_sequence::bigint FROM node_usage_cursors WHERE node_id = 'NL-1'`); got != 1 {
		t.Errorf("acked_sequence %d, ожидалось 1", got)
	}
}

// TestIntegrationUsageAcksOnlyCommitted — §12: агенту подтверждается ровно то, что
// уже закоммичено, иначе он удалит из спула неучтённое.
func TestIntegrationUsageAcksOnlyCommitted(t *testing.T) {
	stack := newUsageStack(t)
	accountingID := seedUsageCustomer(t, stack, 1<<30)

	now := time.Now().UTC()
	stack.agent.set("NL-1",
		batchOf(1, now, nodeagent.UserUsage{AccountingID: accountingID, UplinkBytes: 10, DownlinkBytes: 0}),
		batchOf(2, now, nodeagent.UserUsage{AccountingID: accountingID, UplinkBytes: 20, DownlinkBytes: 0}),
	)
	pullRound(t, stack.usage)

	if got := nodeTotal(t, stack, "NL-1"); got != 30 {
		t.Errorf("расход %d, ожидалось 30", got)
	}

	// Первый опрос уходит с пустым спулом — подтверждать ещё нечего.
	acked := stack.agent.acked["NL-1"]
	if len(acked) != 1 {
		t.Fatalf("опросов NL-1 %d, ожидался 1", len(acked))
	}
	if acked[0].SpoolID != "" || acked[0].Sequence != 0 {
		t.Errorf("первое подтверждение %+v, ожидалось пустое", acked[0])
	}

	// Оба batch закоммичены, поэтому следующий опрос подтверждает последний.
	pullRound(t, stack.usage)

	acked = stack.agent.acked["NL-1"]
	last := acked[len(acked)-1]
	if last.SpoolID != "spool-1" || last.Sequence != 2 {
		t.Errorf("последнее подтверждение %+v, ожидалось spool-1/2", last)
	}
}

// TestIntegrationUsageLeaseIsPerNode — §12: на ноду одновременно активен один pull
// worker.
func TestIntegrationUsageLeaseIsPerNode(t *testing.T) {
	stack := newUsageStack(t)
	seedUsageCustomer(t, stack, 1<<30)
	ctx := context.Background()

	repo := New(stack.pool)
	first, err := repo.ClaimNode(ctx, testUsageOwner, time.Minute, 0)
	if err != nil {
		t.Fatalf("ClaimNode: %v", err)
	}
	if first == nil {
		t.Fatal("первая нода не взята")
	}

	// Вторая реплика не должна получить ту же ноду, пока lease жив.
	for range 5 {
		next, err := repo.ClaimNode(ctx, "другая-реплика", time.Minute, 0)
		if err != nil {
			t.Fatalf("ClaimNode: %v", err)
		}
		if next == nil {
			break
		}
		if next.NodeID == first.NodeID {
			t.Fatalf("нода %s взята второй репликой при живом lease", first.NodeID)
		}
	}

	// Собственный lease переклаймить можно: каждый шаг идёт отдельной транзакцией.
	again, err := repo.ClaimNode(ctx, testUsageOwner, time.Minute, 0)
	if err != nil {
		t.Fatalf("ClaimNode: %v", err)
	}
	if again == nil {
		t.Error("владелец не может продолжить работу со своей нодой")
	}
}

// TestIntegrationUsageReleasesLeaseAfterStep — lease снимается по завершении шага,
// иначе нода простаивала бы до истечения TTL.
func TestIntegrationUsageReleasesLeaseAfterStep(t *testing.T) {
	stack := newUsageStack(t)
	seedUsageCustomer(t, stack, 1<<30)

	pullRound(t, stack.usage)

	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM node_usage_cursors WHERE lease_owner IS NOT NULL`); got != 0 {
		t.Errorf("нод с невыснятым lease %d", got)
	}
}

// TestIntegrationUsageUnavailableNodeChangesNothing — §16: недоступность агента не
// меняет ни desired state, ни counters.
func TestIntegrationUsageUnavailableNodeChangesNothing(t *testing.T) {
	stack := newUsageStack(t)
	seedUsageCustomer(t, stack, 1<<30)

	stack.agent.failWith = nodeagent.CodeUnavailable
	pullRound(t, stack.usage)

	if got := nodeTotal(t, stack, "NL-1"); got != 0 {
		t.Errorf("расход %d, ожидался 0", got)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_accesses WHERE desired_state <> 'PRESENT'`); got != 0 {
		t.Errorf("недоступность ноды погасила %d access", got)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM node_usage_cursors WHERE lease_owner IS NOT NULL`); got != 0 {
		t.Error("lease не снят после неудачного опроса")
	}
}

// ---------------------------------------------------------------------------
// Ретенция реестра дедупа (§12)
// ---------------------------------------------------------------------------

// testRetentionWindow — окно, с которым гоняется прунер в тестах. Значение
// произвольное: важно только, что «свежая» и «состаренная» строки лежат по разные
// стороны от него.
const testRetentionWindow = time.Hour

// pruneDedup выполняет один шаг ретенции и возвращает, осталась ли работа.
func pruneDedup(t *testing.T, stack usageStack, retention time.Duration) bool {
	t.Helper()

	progressed, err := app.NewPruneUsageDedup(New(stack.pool), retention, 1000).
		ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("шаг ретенции: %v", err)
	}
	return progressed
}

func dedupRows(t *testing.T, stack usageStack) int64 {
	t.Helper()
	return scalar[int64](t, stack.pool, `SELECT count(*) FROM traffic_usage_items_processed`)
}

// ageDedupRows состаривает весь реестр: processed_at по умолчанию now(), а
// проверять надо поведение на границе окна.
func ageDedupRows(t *testing.T, stack usageStack, by time.Duration) {
	t.Helper()
	exec(t, stack.pool,
		`UPDATE traffic_usage_items_processed SET processed_at = processed_at - $1::interval`,
		by.String())
}

// seedProcessedItem доводит одну дельту до реестра дедупа.
func seedProcessedItem(t *testing.T, stack usageStack) {
	t.Helper()

	accountingID := seedUsageCustomer(t, stack, 1<<30)
	stack.agent.set("NL-1",
		batchOf(1, time.Now().UTC(), nodeagent.UserUsage{
			AccountingID: accountingID, UplinkBytes: 100, DownlinkBytes: 200,
		}),
	)
	pullRound(t, stack.usage)

	if got := dedupRows(t, stack); got != 1 {
		t.Fatalf("строк реестра %d, ожидалась 1", got)
	}
}

// TestIntegrationRetentionKeepsUnacknowledgedItems — §12: удаляется только то,
// что подтверждено. Неподтверждённый batch агент пришлёт снова, и без своей
// строки реестра он начислится второй раз.
//
// Состояние «обработан, но не подтверждён» — не выдумка теста: между commit
// группы и AdvanceCursor есть окно, и падение воркера в нём оставляет ровно это.
func TestIntegrationRetentionKeepsUnacknowledgedItems(t *testing.T) {
	stack := newUsageStack(t)
	seedProcessedItem(t, stack)

	exec(t, stack.pool, `UPDATE node_usage_cursors SET acked_sequence = 0 WHERE node_id = 'NL-1'`)
	ageDedupRows(t, stack, 2*testRetentionWindow)

	pruneDedup(t, stack, testRetentionWindow)
	if got := dedupRows(t, stack); got != 1 {
		t.Fatalf("строк реестра %d, ожидалась 1 — удалено неподтверждённое", got)
	}

	// Вторая половина проверяет, что предыдущая не прошла по постороннему поводу:
	// та же строка после подтверждения обязана уйти.
	exec(t, stack.pool, `UPDATE node_usage_cursors SET acked_sequence = 1 WHERE node_id = 'NL-1'`)

	pruneDedup(t, stack, testRetentionWindow)
	if got := dedupRows(t, stack); got != 0 {
		t.Errorf("строк реестра %d, ожидалось 0 — подтверждённое не удалено", got)
	}
}

// TestIntegrationRetentionKeepsFreshItems — §12: возраст сам по себе является
// условием. Он покрывает простой backend: подтверждение записано у нас, но до
// агента ещё не доехало.
func TestIntegrationRetentionKeepsFreshItems(t *testing.T) {
	stack := newUsageStack(t)
	seedProcessedItem(t, stack)

	// Строка подтверждена (pullRound сдвинул курсор), но моложе окна.
	pruneDedup(t, stack, testRetentionWindow)
	if got := dedupRows(t, stack); got != 1 {
		t.Fatalf("строк реестра %d, ожидалась 1 — удалено моложе окна", got)
	}

	ageDedupRows(t, stack, 2*testRetentionWindow)

	pruneDedup(t, stack, testRetentionWindow)
	if got := dedupRows(t, stack); got != 0 {
		t.Errorf("строк реестра %d, ожидалось 0 — старое не удалено", got)
	}
}

// TestIntegrationRetentionDropsVanishedSpool — §12: строки исчезнувшего спула
// удаляются, даже если их sequence выше подтверждённого.
//
// Сравнивать их с acked_sequence бессмысленно: новый спул начинает нумерацию с
// нуля, поэтому по одному только sequence такие строки лежали бы вечно.
func TestIntegrationRetentionDropsVanishedSpool(t *testing.T) {
	stack := newUsageStack(t)
	seedProcessedItem(t, stack)

	// Агент переустановлен: новый spool_id, нумерация с нуля.
	exec(t, stack.pool,
		`UPDATE node_usage_cursors SET spool_id = 'spool-2', acked_sequence = 0 WHERE node_id = 'NL-1'`)
	ageDedupRows(t, stack, 2*testRetentionWindow)

	pruneDedup(t, stack, testRetentionWindow)
	if got := dedupRows(t, stack); got != 0 {
		t.Errorf("строк реестра %d, ожидалось 0 — строки исчезнувшего спула остались", got)
	}
}

// TestIntegrationRetentionLateRetryReaccrues — §18, fault test: очень поздний
// повтор после очистки дедупа начисляет трафик ВТОРОЙ раз.
//
// Тест закрепляет принятую погрешность, а не желаемое поведение. §12 называет её
// положительной (в пользу сервиса) и допустимой; цена альтернативы — реестр,
// растущий пропорционально трафику без ограничения.
//
// Обратная сторона того же утверждения проверена соседями: пока строка реестра
// жива, тот же повтор — no-op (TestIntegrationUsageDeduplicatesRepeatedBatch).
func TestIntegrationRetentionLateRetryReaccrues(t *testing.T) {
	stack := newUsageStack(t)
	seedProcessedItem(t, stack)

	if got := nodeTotal(t, stack, "NL-1"); got != 300 {
		t.Fatalf("расход NL-1 %d, ожидалось 300", got)
	}

	ageDedupRows(t, stack, 2*testRetentionWindow)
	pruneDedup(t, stack, testRetentionWindow)
	if got := dedupRows(t, stack); got != 0 {
		t.Fatalf("строк реестра %d, ожидалось 0 — ретенция не сработала", got)
	}

	// Агент так и не получил подтверждения и присылает тот же batch снова.
	exec(t, stack.pool, `UPDATE node_usage_cursors SET acked_sequence = 0 WHERE node_id = 'NL-1'`)
	pullRound(t, stack.usage)

	if got := nodeTotal(t, stack, "NL-1"); got != 600 {
		t.Errorf("расход NL-1 %d, ожидалось 600: после очистки дедупа повтор обязан "+
			"начислиться второй раз — это принятая погрешность §12, а не ошибка", got)
	}
}
