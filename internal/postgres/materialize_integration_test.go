package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
)

// Интеграционные тесты воркера материализации. Смысл слоя в SQL: взятие lease
// под SKIP LOCKED, курсор, порядок записи и то, что ретайр не удаляет строк.

var _ app.MaterializationRepository = (*Repository)(nil)

const testWorkerOwner = "worker-integration"

// materializeStack — все три use case поверх одного пула и одного шифра.
type materializeStack struct {
	manifest    *app.ApplyFleetManifest
	customer    *app.ApplyCustomerAccess
	materialize *app.MaterializeManifest
	links       *app.GetCustomerAccessLinks
	pool        *pgxpool.Pool
}

func newMaterializeStack(t *testing.T) materializeStack {
	t.Helper()

	customer, pool := newFixture(t)
	cipher := testCipher(t)
	repo := New(pool)

	return materializeStack{
		manifest: app.NewApplyFleetManifest(repo),
		customer: customer,
		materialize: app.NewMaterializeManifest(
			repo, crypto.NewGenerator(), cipher, testWorkerOwner, time.Minute),
		links: app.NewGetCustomerAccessLinks(repo, cipher),
		pool:  pool,
	}
}

// drainMaterialization крутит воркер до исчерпания работы и возвращает число
// сделанных шагов. Ограничение сверху — защита от бесконечного цикла в тесте.
func drainMaterialization(t *testing.T, uc *app.MaterializeManifest) int {
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

	t.Fatal("воркер не сошёлся за 100 шагов")
	return steps
}

// seedCustomerOnManifest принимает манифест и заводит на нём одного customer.
func seedCustomerOnManifest(t *testing.T, stack materializeStack) {
	t.Helper()

	applyManifest(t, stack.manifest, manifestFixture(7), false)

	expiresAt := time.Now().UTC().Add(30 * 24 * time.Hour)
	if err := stack.customer.Execute(context.Background(), command(1, 1<<30, expiresAt)); err != nil {
		t.Fatalf("ApplyCustomerAccess: %v", err)
	}

	// Джобу первого манифеста досушиваем сразу: customer появился уже после неё,
	// и её обход его не касается.
	drainMaterialization(t, stack.materialize)
}

// TestIntegrationMaterializeCompletesJob — обход доходит до конца и закрывает
// джобу, снимая lease.
func TestIntegrationMaterializeCompletesJob(t *testing.T) {
	stack := newMaterializeStack(t)
	seedCustomerOnManifest(t, stack)

	if got := scalar[string](t, stack.pool,
		`SELECT status FROM manifest_materialization_jobs WHERE revision = 7`); got != "DONE" {
		t.Errorf("статус джобы %q, ожидался DONE", got)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM manifest_materialization_jobs
		 WHERE revision = 7 AND (lease_owner IS NOT NULL OR lease_expires_at IS NOT NULL)`); got != 0 {
		t.Error("lease завершённой джобы не снят")
	}
}

// TestIntegrationMaterializeAddsNode — новая нода даёт FREEDOM всем
// неистёкшим customer fleet вместе с нулевой строкой расхода и операцией.
func TestIntegrationMaterializeAddsNode(t *testing.T) {
	stack := newMaterializeStack(t)
	seedCustomerOnManifest(t, stack)

	grown := manifestFixture(8)
	grown.Nodes = append(grown.Nodes, manifestTestNode("FR-1"))
	grown.Fleets[0].NodeIDs = append(grown.Fleets[0].NodeIDs, "FR-1")
	applyManifest(t, stack.manifest, grown, false)

	drainMaterialization(t, stack.materialize)

	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_accesses
		 WHERE customer_id = $1 AND entry_node_id = 'FR-1' AND retired_at IS NULL`,
		testCustomerID); got != 1 {
		t.Errorf("access на новой ноде %d, ожидался 1", got)
	}
	if got := scalar[string](t, stack.pool,
		`SELECT desired_state FROM vpn_accesses WHERE entry_node_id = 'FR-1'`); got != "PRESENT" {
		t.Errorf("desired_state %q, ожидался PRESENT", got)
	}
	// Новая нода получает нулевой node_quota_usage текущего периода.
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM node_quota_usage u
		 JOIN quota_periods p ON p.quota_period_id = u.quota_period_id
		 WHERE u.node_id = 'FR-1' AND p.closed_at IS NULL AND u.total_bytes = 0`); got != 1 {
		t.Errorf("строк расхода на новой ноде %d, ожидалась 1", got)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations
		 WHERE node_id = 'FR-1' AND operation_type = 'ENSURE_PRESENT' AND status = 'PENDING'`); got != 1 {
		t.Errorf("операций на новую ноду %d, ожидалась 1", got)
	}
}

// TestIntegrationMaterializeIsIdempotent — повторная материализация одной
// revision ничего не добавляет.
func TestIntegrationMaterializeIsIdempotent(t *testing.T) {
	stack := newMaterializeStack(t)
	seedCustomerOnManifest(t, stack)

	grown := manifestFixture(8)
	grown.Nodes = append(grown.Nodes, manifestTestNode("FR-1"))
	grown.Fleets[0].NodeIDs = append(grown.Fleets[0].NodeIDs, "FR-1")
	applyManifest(t, stack.manifest, grown, false)
	drainMaterialization(t, stack.materialize)

	accessesBefore := scalar[int64](t, stack.pool, `SELECT count(*) FROM vpn_accesses`)
	operationsBefore := scalar[int64](t, stack.pool, `SELECT count(*) FROM agent_operations`)

	// Та же revision, поставленная заново: воркер обязан пройти обход вхолостую.
	exec(t, stack.pool,
		`UPDATE manifest_materialization_jobs SET status = 'PENDING', cursor = NULL WHERE revision = 8`)
	drainMaterialization(t, stack.materialize)

	if got := scalar[int64](t, stack.pool, `SELECT count(*) FROM vpn_accesses`); got != accessesBefore {
		t.Errorf("access стало %d, было %d", got, accessesBefore)
	}
	if got := scalar[int64](t, stack.pool, `SELECT count(*) FROM agent_operations`); got != operationsBefore {
		t.Errorf("операций стало %d, было %d", got, operationsBefore)
	}
}

// TestIntegrationMaterializeRetiresLiveNode — удаление связи при живой
// входной ноде ретайрит access и создаёт Remove на неё.
func TestIntegrationMaterializeRetiresLiveNode(t *testing.T) {
	stack := newMaterializeStack(t)
	seedCustomerOnManifest(t, stack)

	shrunk := manifestFixture(8)
	shrunk.Fleets[0].Bridges = nil
	applyManifest(t, stack.manifest, shrunk, true)

	drainMaterialization(t, stack.materialize)

	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_accesses WHERE kind = 'BRIDGE' AND retired_at IS NOT NULL`); got != 1 {
		t.Errorf("ретайрнутых BRIDGE %d, ожидался 1", got)
	}
	// Строка не удаляется: по ней приходит поздний traffic.
	if got := scalar[int64](t, stack.pool, `SELECT count(*) FROM vpn_accesses WHERE kind = 'BRIDGE'`); got != 1 {
		t.Errorf("строк BRIDGE %d, ожидалась 1 — история не удаляется", got)
	}
	if got := scalar[string](t, stack.pool,
		`SELECT apply_state FROM vpn_accesses WHERE kind = 'BRIDGE'`); got != "PENDING" {
		t.Errorf("apply_state %q, ожидался PENDING: удаление ещё предстоит доставить", got)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations
		 WHERE node_id = 'NL-1' AND operation_type = 'ENSURE_ABSENT'`); got != 1 {
		t.Errorf("операций удаления %d, ожидалась 1", got)
	}
}

// TestIntegrationMaterializeRetiresDeadNode — на глобально удалённую ноду не
// уходит ни одной команды, а её незавершённые операции становятся SUPERSEDED.
func TestIntegrationMaterializeRetiresDeadNode(t *testing.T) {
	stack := newMaterializeStack(t)
	seedCustomerOnManifest(t, stack)

	// До удаления у FREEDOM DE-1 висит PENDING-операция от ApplyCustomerAccess.
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations WHERE node_id = 'DE-1' AND status = 'PENDING'`); got != 1 {
		t.Fatalf("подготовка: операций на DE-1 %d, ожидалась 1", got)
	}

	shrunk := manifestFixture(8)
	shrunk.Nodes = []domain.ManifestNode{manifestTestNode("NL-1")}
	shrunk.Fleets[0].NodeIDs = []domain.NodeID{"NL-1"}
	shrunk.Fleets[0].Bridges = nil
	applyManifest(t, stack.manifest, shrunk, true)

	drainMaterialization(t, stack.materialize)

	// Ни одной новой операции на исчезнувшую ноду.
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations
		 WHERE node_id = 'DE-1' AND operation_type = 'ENSURE_ABSENT'`); got != 0 {
		t.Errorf("создано %d команд на глобально удалённую ноду", got)
	}
	// Прежние pending — superseded.
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations WHERE node_id = 'DE-1' AND status = 'PENDING'`); got != 0 {
		t.Errorf("на удалённой ноде осталось %d PENDING-операций", got)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations WHERE node_id = 'DE-1' AND status = 'SUPERSEDED'`); got != 1 {
		t.Errorf("SUPERSEDED-операций %d, ожидалась 1", got)
	}
	// Доставлять нечего, значит состояние доставки достигнуто.
	if got := scalar[string](t, stack.pool,
		`SELECT apply_state FROM vpn_accesses
		 WHERE kind = 'FREEDOM' AND logical_target_key = 'DE-1'`); got != "APPLIED" {
		t.Errorf("apply_state %q, ожидался APPLIED", got)
	}
}

// TestIntegrationMaterializeRepoint — смена egress_tag меняет цель выхода,
// сохраняя credentials и поколение.
func TestIntegrationMaterializeRepoint(t *testing.T) {
	stack := newMaterializeStack(t)
	seedCustomerOnManifest(t, stack)

	accountingBefore := scalar[string](t, stack.pool, `SELECT accounting_id FROM vpn_accesses WHERE kind = 'BRIDGE'`)

	repointed := manifestFixture(8)
	repointed.Fleets[0].Bridges[0].EgressTag = "de-exit-v2"
	applyManifest(t, stack.manifest, repointed, false)

	drainMaterialization(t, stack.materialize)

	if got := scalar[string](t, stack.pool, `SELECT egress_key FROM vpn_accesses WHERE kind = 'BRIDGE'`); got != "de-exit-v2" {
		t.Errorf("egress_key %q, ожидался de-exit-v2", got)
	}
	if got := scalar[string](t, stack.pool, `SELECT accounting_id FROM vpn_accesses WHERE kind = 'BRIDGE'`); got != accountingBefore {
		t.Error("repoint сменил accounting_id — это должно быть тем же поколением")
	}
	if got := scalar[int32](t, stack.pool, `SELECT generation FROM vpn_accesses WHERE kind = 'BRIDGE'`); got != 1 {
		t.Errorf("поколение %d, ожидалось 1", got)
	}
	if got := scalar[int64](t, stack.pool, `SELECT count(*) FROM vpn_accesses WHERE kind = 'BRIDGE'`); got != 1 {
		t.Error("repoint создал новый access вместо обновления существующего")
	}
	// Переиздание правила доставляется обычным EnsureUserPresent.
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM agent_operations o
		 JOIN vpn_accesses a ON a.access_id = o.access_id
		 WHERE a.kind = 'BRIDGE' AND o.operation_type = 'ENSURE_PRESENT' AND o.status = 'PENDING'`); got != 1 {
		t.Errorf("PENDING-операций repoint %d, ожидалась 1", got)
	}
}

// TestIntegrationMaterializeCursorWalksAllCustomers — обход идёт по
// всем customer, по одному за шаг.
func TestIntegrationMaterializeCursorWalksAllCustomers(t *testing.T) {
	stack := newMaterializeStack(t)
	seedCustomerOnManifest(t, stack)

	// Второй customer на том же fleet.
	second := command(1, 1<<30, time.Now().UTC().Add(30*24*time.Hour))
	second.Command.CustomerID = "customer-integration-2"
	if err := stack.customer.Execute(context.Background(), second); err != nil {
		t.Fatalf("второй ApplyCustomerAccess: %v", err)
	}

	grown := manifestFixture(8)
	grown.Nodes = append(grown.Nodes, manifestTestNode("FR-1"))
	grown.Fleets[0].NodeIDs = append(grown.Fleets[0].NodeIDs, "FR-1")
	applyManifest(t, stack.manifest, grown, false)

	// Два customer плюс завершающий шаг, закрывающий джобу.
	if steps := drainMaterialization(t, stack.materialize); steps != 3 {
		t.Errorf("шагов %d, ожидалось 3 (два customer и закрытие джобы)", steps)
	}

	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_accesses WHERE entry_node_id = 'FR-1' AND retired_at IS NULL`); got != 2 {
		t.Errorf("access на новой ноде %d, ожидалось 2 — по одному на customer", got)
	}
}

// TestIntegrationMaterializeSkipsExpiredCustomer — ограничивает создание
// неистёкшими customer.
func TestIntegrationMaterializeSkipsExpiredCustomer(t *testing.T) {
	stack := newMaterializeStack(t)
	seedCustomerOnManifest(t, stack)

	exec(t, stack.pool,
		`UPDATE customer_entitlements SET expires_at = now() - interval '1 hour' WHERE customer_id = $1`,
		testCustomerID)

	grown := manifestFixture(8)
	grown.Nodes = append(grown.Nodes, manifestTestNode("FR-1"))
	grown.Fleets[0].NodeIDs = append(grown.Fleets[0].NodeIDs, "FR-1")
	applyManifest(t, stack.manifest, grown, false)

	drainMaterialization(t, stack.materialize)

	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_accesses WHERE entry_node_id = 'FR-1'`); got != 0 {
		t.Errorf("истёкшему customer создано %d access на новой ноде", got)
	}
}

// TestIntegrationMaterializeFeedsCustomerLinks — путь целиком: манифест →
// материализация → выданная ссылка на новой ноде.
func TestIntegrationMaterializeFeedsCustomerLinks(t *testing.T) {
	stack := newMaterializeStack(t)
	seedCustomerOnManifest(t, stack)

	grown := manifestFixture(8)
	grown.Nodes = append(grown.Nodes, manifestTestNode("FR-1"))
	grown.Fleets[0].NodeIDs = append(grown.Fleets[0].NodeIDs, "FR-1")
	applyManifest(t, stack.manifest, grown, false)
	drainMaterialization(t, stack.materialize)

	// apply_state проставляет dispatcher, которого ещё нет.
	exec(t, stack.pool, `UPDATE vpn_accesses SET apply_state = 'APPLIED' WHERE customer_id = $1`, testCustomerID)

	links, err := stack.links.Execute(context.Background(), testCustomerID)
	if err != nil {
		t.Fatalf("GetCustomerAccessLinks: %v", err)
	}
	if len(links) != 4 {
		t.Fatalf("ссылок %d, ожидалось 4 (3 прежних + новая нода)", len(links))
	}
	for _, link := range links {
		if link.Status.State != domain.LinkStateReady {
			t.Fatalf("состояние %s, ожидалось READY", link.Status.State)
		}
	}
}

// Fault tests материализации. Проверяют не то, что воркер делает, а то,
// что остаётся после его внезапной смерти.

// errWorkerCrashed изображает смерть процесса на середине шага.
var errWorkerCrashed = errors.New("воркер умер")

// crashingMaterialization роняет шаг в единственной интересной точке: customer
// уже изменён, курсор ещё не сдвинут.
//
// Обёртка вокруг порта, а не отдельный fake: транзакция, откат которой
// проверяется, должна быть настоящей, postgres'овой. Fake откатил бы ровно то,
// что ему велели, и доказывал бы сам себя.
type crashingMaterialization struct {
	inner app.MaterializationRepository
	// armed разоружается первым же падением: после «перезапуска» тот же самый
	// репозиторий обязан довести работу до конца.
	armed bool
}

func (r *crashingMaterialization) WithinMaterializationTx(
	ctx context.Context,
	fn func(app.MaterializationTx) error,
) error {
	return r.inner.WithinMaterializationTx(ctx, func(tx app.MaterializationTx) error {
		return fn(&crashingMaterializationTx{MaterializationTx: tx, repo: r})
	})
}

type crashingMaterializationTx struct {
	app.MaterializationTx
	repo *crashingMaterialization
}

// AdvanceCursor сдвигает курсор по-настоящему и лишь ПОТОМ роняет шаг.
//
// Порядок именно такой, чтобы утверждение о курсоре не было самосбывающимся:
// упади обёртка раньше вызова, курсор остался бы пустым просто потому, что его
// никто не двигал. Падение после сдвига требует от отката настоящей работы.
func (t *crashingMaterializationTx) AdvanceCursor(ctx context.Context, revision int64, customerID string) error {
	if err := t.MaterializationTx.AdvanceCursor(ctx, revision, customerID); err != nil {
		return err
	}
	if t.repo.armed {
		t.repo.armed = false
		return errWorkerCrashed
	}
	return nil
}

// TestIntegrationMaterializeCrashLeavesNoPartialCustomer — смерть воркера
// посреди customer не оставляет ни половины изменений, ни съеденной работы.
//
// Всё держится на решении 34: курсор двигается той же транзакцией, что и
// изменения. Поэтому «сделано, но не отмечено» существовать не может, и
// отдельная сверка после рестарта не нужна.
func TestIntegrationMaterializeCrashLeavesNoPartialCustomer(t *testing.T) {
	stack := newMaterializeStack(t)
	seedCustomerOnManifest(t, stack)

	// Новая нода: шагу, который сейчас упадёт, есть что записать.
	grown := manifestFixture(8)
	grown.Nodes = append(grown.Nodes, manifestTestNode("FR-1"))
	grown.Fleets[0].NodeIDs = append(grown.Fleets[0].NodeIDs, "FR-1")
	applyManifest(t, stack.manifest, grown, false)

	crashing := &crashingMaterialization{inner: New(stack.pool), armed: true}
	worker := app.NewMaterializeManifest(
		crashing, crypto.NewGenerator(), testCipher(t), testWorkerOwner, time.Minute)

	if _, err := worker.ProcessNext(context.Background()); !errors.Is(err, errWorkerCrashed) {
		t.Fatalf("ошибка шага %v, ожидалась смерть воркера", err)
	}

	// Откат забирает всё: и запись customer, и lease джобы, взятый той же
	// транзакцией. Простаивать до истечения TTL после отката нечему.
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_accesses WHERE entry_node_id = 'FR-1'`); got != 0 {
		t.Errorf("access на новой ноде %d, ожидалось 0 — половина шага уцелела", got)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM manifest_materialization_jobs
		 WHERE revision = 8 AND cursor IS NOT NULL`); got != 0 {
		t.Errorf("курсор джобы сдвинут %d раз, ожидалось 0 — работа съедена", got)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM manifest_materialization_jobs
		 WHERE revision = 8 AND lease_owner IS NOT NULL`); got != 0 {
		t.Errorf("lease джобы уцелел %d раз, ожидалось 0", got)
	}

	// Перезапуск доводит ровно ту же работу до конца ровно один раз.
	drainMaterialization(t, stack.materialize)

	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_accesses WHERE entry_node_id = 'FR-1' AND retired_at IS NULL`); got != 1 {
		t.Errorf("access на новой ноде после перезапуска %d, ожидался 1", got)
	}
	if got := scalar[string](t, stack.pool,
		`SELECT status FROM manifest_materialization_jobs WHERE revision = 8`); got != "DONE" {
		t.Errorf("статус джобы %q, ожидался DONE", got)
	}
}

// TestIntegrationMaterializeResumesFromCursorAfterCrash — подобранная джоба
// продолжает обход с курсора, а не с начала.
//
// Цена ошибки здесь не в порче данных — материализация идемпотентна, — а в том,
// что каждый рестарт заново прогонял бы всех customer. При тысячах customer это
// разница между «доделал хвост» и «начал сначала».
func TestIntegrationMaterializeResumesFromCursorAfterCrash(t *testing.T) {
	stack := newMaterializeStack(t)
	seedCustomerOnManifest(t, stack)

	const secondCustomerID = "customer-integration-2"
	second := command(1, 1<<30, time.Now().UTC().Add(30*24*time.Hour))
	second.Command.CustomerID = secondCustomerID
	if err := stack.customer.Execute(context.Background(), second); err != nil {
		t.Fatalf("второй ApplyCustomerAccess: %v", err)
	}

	grown := manifestFixture(8)
	grown.Nodes = append(grown.Nodes, manifestTestNode("FR-1"))
	grown.Fleets[0].NodeIDs = append(grown.Fleets[0].NodeIDs, "FR-1")
	applyManifest(t, stack.manifest, grown, false)

	// Первый шаг проходит целиком: первый customer записан, курсор сдвинут.
	if progressed, err := stack.materialize.ProcessNext(context.Background()); err != nil {
		t.Fatalf("первый шаг: %v", err)
	} else if !progressed {
		t.Fatal("первый шаг не нашёл работы")
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_accesses WHERE entry_node_id = 'FR-1'`); got != 1 {
		t.Fatalf("подготовка: access на новой ноде %d, ожидался 1", got)
	}

	// Тут процесс умер, не сняв lease: транзакция шага уже закоммичена, а джоба
	// осталась в IN_PROGRESS. Ждать TTL в тесте нечего, поэтому lease протухает
	// сразу.
	exec(t, stack.pool,
		`UPDATE manifest_materialization_jobs SET lease_expires_at = now() - interval '1 second'
		 WHERE revision = 8`)

	// Джобу подбирает другой владелец — так видно, что взята именно чужая.
	restarted := app.NewMaterializeManifest(
		New(stack.pool), crypto.NewGenerator(), testCipher(t), "worker-integration-2", time.Minute)
	if progressed, err := restarted.ProcessNext(context.Background()); err != nil {
		t.Fatalf("шаг после перезапуска: %v", err)
	} else if !progressed {
		t.Fatal("перезапущенный воркер не подобрал осиротевшую джобу")
	}

	// Единственный шаг после перезапуска достался ВТОРОМУ customer. Начнись обход
	// с начала, первый customer прошёл бы вхолостую, а этой строки бы не было.
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_accesses WHERE entry_node_id = 'FR-1' AND customer_id = $1`,
		secondCustomerID); got != 1 {
		t.Errorf("access второго customer %d, ожидался 1 — обход начался сначала", got)
	}
	if got := scalar[int64](t, stack.pool,
		`SELECT count(*) FROM vpn_accesses WHERE entry_node_id = 'FR-1' AND customer_id = $1`,
		testCustomerID); got != 1 {
		t.Errorf("access первого customer %d, ожидался 1", got)
	}
}
