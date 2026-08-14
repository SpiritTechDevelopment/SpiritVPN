package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/lib/pq"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	"github.com/RomanRyabinkin/SpiritVPN/internal/migrations"
)

// Интеграционные тесты адаптера идут против настоящего PostgreSQL: смысл этого
// слоя целиком в SQL — порядок FOR UPDATE, partial unique index на единственный
// открытый период, unique (access_id, desired_version) как assertion и диапазон
// numeric(20,0). Юнит-тест маппинга не проверил бы ничего из этого.
//
// Гейт — SPIRITVPN_INTEGRATION_TESTS вместе с DATABASE_URL; без них тесты
// пропускаются, и обычный `go test ./...` остаётся полностью офлайновым.

// Проверки на этапе компиляции: адаптер действительно закрывает порты.
var (
	_ app.ApplyRepository = (*Repository)(nil)
	_ app.ApplyTx         = (*applyTx)(nil)
)

var testDSN string

func TestMain(m *testing.M) {
	if os.Getenv("SPIRITVPN_INTEGRATION_TESTS") == "" {
		// Юнит-тесты пакета (numeric) работают и без базы.
		os.Exit(m.Run())
	}

	testDSN = os.Getenv("DATABASE_URL")
	if testDSN == "" {
		fmt.Fprintln(os.Stderr, "SPIRITVPN_INTEGRATION_TESTS задан, но DATABASE_URL пуст")
		os.Exit(1)
	}

	if err := migrateUp(testDSN); err != nil {
		fmt.Fprintln(os.Stderr, "миграции:", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// migrateUp накатывает схему тем же мигратором, что и деплой: тест обязан идти по
// той же схеме, из которой sqlc сгенерировал код.
func migrateUp(dsn string) (migrateErr error) {
	sqlDB, err := sql.Open("postgres", dsn)
	if err != nil {
		return err
	}
	defer func() {
		migrateErr = errors.Join(migrateErr, sqlDB.Close())
	}()

	m, err := migrations.New(sqlDB)
	if err != nil {
		return err
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

// --- обвязка ----------------------------------------------------------------

const (
	testFleetID    = 42
	testCustomerID = "customer-integration"
	// testApplyActor — идентичность product-сервиса из mTLS; уезжает в
	// actor_id записи аудита.
	testApplyActor = "product-svc"
)

// truncatedTables перечисляются в порядке, обратном зависимостям; CASCADE снял бы
// вопрос, но явный список ловит забытую в будущем таблицу.
var truncatedTables = []string{
	"agent_operations",
	"traffic_usage_items_processed",
	"traffic_batch_quarantine",
	"node_usage_cursors",
	"vpn_accesses",
	"node_quota_usage",
	"quota_periods",
	"customer_entitlements",
	"vpn_bridge_routes",
	"vpn_fleet_nodes",
	"manifest_materialization_jobs",
	"vpn_nodes",
	"vpn_fleets",
	"manifest_revisions",
	"audit_events",
}

// newFixture поднимает чистую базу и собранный на настоящих адаптерах use case.
func newFixture(t *testing.T) (*app.ApplyCustomerAccess, *pgxpool.Pool) {
	t.Helper()

	if os.Getenv("SPIRITVPN_INTEGRATION_TESTS") == "" {
		t.Skip("интеграционный тест: задайте SPIRITVPN_INTEGRATION_TESTS и DATABASE_URL")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	for _, table := range truncatedTables {
		if _, err := pool.Exec(ctx, "TRUNCATE TABLE "+table+" CASCADE"); err != nil {
			t.Fatalf("TRUNCATE %s: %v", table, err)
		}
	}

	// Настоящие crypto-адаптеры, а не заглушки: тест проверяет и то, что
	// зашифрованный client_uuid укладывается в bytea-колонку.
	return app.NewApplyCustomerAccess(New(pool), crypto.NewGenerator(), testCipher(t)), pool
}

// testCipher собирает шифр на детерминированном ключе. Ключ один и тот же во всех
// вызовах, поэтому read-путь читает ровно то, что запечатал командный.
func testCipher(t *testing.T) *crypto.Cipher {
	t.Helper()

	key, err := crypto.NewKey("test-key", make([]byte, crypto.KeySize))
	if err != nil {
		t.Fatalf("crypto.NewKey: %v", err)
	}
	cipher, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatalf("crypto.NewCipher: %v", err)
	}
	return cipher
}

// seedTopology создаёт проекцию manifest: revision, fleet, ноды и bridge-связи.
func seedTopology(t *testing.T, pool *pgxpool.Pool, nodes []string, bridges [][4]string) {
	t.Helper()
	ctx := context.Background()

	exec := func(sqlText string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sqlText, args...); err != nil {
			t.Fatalf("seed %q: %v", sqlText, err)
		}
	}

	exec(`INSERT INTO manifest_revisions (revision, digest, canonical_payload)
	      VALUES (1, 'test-digest', '{}'::jsonb)`)
	exec(`INSERT INTO vpn_fleets (vpn_fleet_id, manifest_revision) VALUES ($1, 1)`, int64(testFleetID))

	for _, node := range nodes {
		exec(`INSERT INTO vpn_nodes (node_id, agent_config, public_config, manifest_revision)
		      VALUES ($1, '{}'::jsonb, '{}'::jsonb, 1)`, node)
		exec(`INSERT INTO vpn_fleet_nodes (vpn_fleet_id, node_id, manifest_revision)
		      VALUES ($1, $2, 1)`, int64(testFleetID), node)
	}

	// bridge: {routing_key, entry_node_id, exit_node_id, egress_tag}
	for _, bridge := range bridges {
		exec(`INSERT INTO vpn_bridge_routes
		        (vpn_fleet_id, routing_key, entry_node_id, exit_node_id, egress_tag, display_name, manifest_revision)
		      VALUES ($1, $2, $3, $4, $5, $2, 1)`,
			int64(testFleetID), bridge[0], bridge[1], bridge[2], bridge[3])
	}
}

// command собирает команду так же, как её соберёт gRPC-хендлер: из
// expires_at_epoch_sec, то есть с секундной точностью.
//
// Усечение здесь не косметика. timestamptz хранит микросекунды, поэтому время с
// наносекундами, записанное и прочитанное обратно, оказывается строго меньше
// исходного — и точный повтор команды классифицировался бы как renewal:
// открывался бы новый период квоты, а накопленный трафик сбрасывался. На проводе
// такого значения не бывает по типу поля, но тест обязан строить команду так же,
// как её строит граница системы, иначе он проверяет несуществующий сценарий.
//
// Возвращает app-команду, а не доменную: actor и request_id уезжают в
// audit_events, и тест обязан подавать их так же, как транспорт.
func command(commandNumber uint64, quotaBytes uint64, expiresAt time.Time) app.ApplyCustomerCommand {
	return app.ApplyCustomerCommand{
		Command: domain.ApplyCommand{
			CustomerID:      testCustomerID,
			FleetID:         testFleetID,
			UsageQuotaBytes: quotaBytes,
			ExpiresAt:       time.Unix(expiresAt.Unix(), 0).UTC(),
			CommandNumber:   commandNumber,
		},
		Actor:     testApplyActor,
		RequestID: "req-" + strconv.FormatUint(commandNumber, 10),
	}
}

func scalar[T any](t *testing.T, pool *pgxpool.Pool, sqlText string, args ...any) T {
	t.Helper()
	var value T
	if err := pool.QueryRow(context.Background(), sqlText, args...).Scan(&value); err != nil {
		t.Fatalf("запрос %q: %v", sqlText, err)
	}
	return value
}

// --- тесты ------------------------------------------------------------------

// Первый Apply создаёт корневую строку, открывает период, заводит нулевой расход на
// каждой ноде fleet, материализует access под топологию и кладёт операции в outbox.
func TestIntegrationApplyCreatesCustomer(t *testing.T) {
	uc, pool := newFixture(t)
	seedTopology(t, pool, []string{"node-a", "node-b"}, [][4]string{
		{"a-to-b", "node-a", "node-b", "exit-b"},
	})

	expiresAt := time.Now().UTC().Add(30 * 24 * time.Hour)
	if err := uc.Execute(context.Background(), command(1, 1<<30, expiresAt)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if got := scalar[int64](t, pool,
		`SELECT last_command_number FROM customer_entitlements WHERE customer_id = $1`,
		testCustomerID); got != 1 {
		t.Fatalf("last_command_number = %d, ожидалось 1", got)
	}

	// Ровно один открытый период; partial unique index это же и гарантирует.
	if got := scalar[int64](t, pool,
		`SELECT count(*) FROM quota_periods WHERE customer_id = $1 AND closed_at IS NULL`,
		testCustomerID); got != 1 {
		t.Fatalf("открытых периодов %d, ожидался 1", got)
	}

	// Нулевой расход на каждой ноде fleet.
	if got := scalar[int64](t, pool,
		`SELECT count(*) FROM node_quota_usage
		 WHERE total_bytes = 0 AND exhausted_at IS NULL`); got != 2 {
		t.Fatalf("строк расхода %d, ожидалось 2", got)
	}

	// link_count = fleet_node_count + bridge_relation_count.
	if got := scalar[int64](t, pool,
		`SELECT count(*) FROM vpn_accesses WHERE customer_id = $1`, testCustomerID); got != 3 {
		t.Fatalf("access %d, ожидалось 3", got)
	}
	if got := scalar[int64](t, pool,
		`SELECT count(*) FROM vpn_accesses
		 WHERE customer_id = $1 AND desired_state = 'PRESENT' AND apply_state = 'PENDING'`,
		testCustomerID); got != 3 {
		t.Fatalf("PRESENT/PENDING access %d, ожидалось 3", got)
	}

	// BRIDGE несёт egress_tag дословно, FREEDOM — локальный выход.
	if got := scalar[string](t, pool,
		`SELECT egress_key FROM vpn_accesses WHERE customer_id = $1 AND kind = 'BRIDGE'`,
		testCustomerID); got != "exit-b" {
		t.Fatalf("egress_key BRIDGE = %q, ожидалось exit-b", got)
	}
	if got := scalar[int64](t, pool,
		`SELECT count(*) FROM vpn_accesses WHERE kind = 'FREEDOM' AND egress_key = ''`); got != 2 {
		t.Fatalf("FREEDOM с локальным выходом %d, ожидалось 2", got)
	}

	// Операции лежат в outbox готовыми к отправке.
	if got := scalar[int64](t, pool,
		`SELECT count(*) FROM agent_operations
		 WHERE operation_type = 'ENSURE_PRESENT' AND status = 'PENDING'
		   AND desired_version = 1 AND next_attempt_at IS NOT NULL`); got != 3 {
		t.Fatalf("операций %d, ожидалось 3", got)
	}

	// desired_revision увеличен ровно один раз на ноду, несмотря на два access у
	// node-a: FREEDOM и BRIDGE.
	if got := scalar[int64](t, pool,
		`SELECT desired_revision FROM vpn_nodes WHERE node_id = 'node-a'`); got != 2 {
		t.Fatalf("desired_revision node-a = %d, ожидалось 2 (1 стартовое + 1)", got)
	}

	// client_uuid хранится только зашифрованным.
	if got := scalar[int64](t, pool,
		`SELECT count(*) FROM vpn_accesses
		 WHERE octet_length(encrypted_client_uuid) = $1 AND encryption_key_id = 'test-key'`,
		int32(crypto.SealedBlobSize)); got != 3 {
		t.Fatalf("зашифрованных credentials %d, ожидалось 3", got)
	}
}

// Точный повтор принятой команды с бо́льшим номером не создаёт ни новых операций, ни
// нового периода, но двигает last_command_number.
func TestIntegrationApplyRepeatIsNoOp(t *testing.T) {
	uc, pool := newFixture(t)
	seedTopology(t, pool, []string{"node-a"}, nil)

	ctx := context.Background()
	expiresAt := time.Now().UTC().Add(30 * 24 * time.Hour)

	if err := uc.Execute(ctx, command(1, 1<<30, expiresAt)); err != nil {
		t.Fatalf("первый Execute: %v", err)
	}
	periodID := scalar[string](t, pool,
		`SELECT quota_period_id::text FROM quota_periods WHERE customer_id = $1`, testCustomerID)

	// expires_at обязан прочитаться ровно тем же значением, каким записан. Иначе
	// сохранённое окажется меньше приходящего, ClassifyApply увидит renewal вместо
	// повтора, и каждая повторная команда сбрасывала бы счётчики трафика. Проверка
	// стоит здесь, а не в комментарии, потому что нарушается она молча: типом
	// колонки (timestamptz — микросекунды) или изменением границы системы.
	stored := scalar[time.Time](t, pool,
		`SELECT expires_at FROM customer_entitlements WHERE customer_id = $1`, testCustomerID)
	if sent := command(1, 1<<30, expiresAt).Command.ExpiresAt; !stored.Equal(sent) {
		t.Fatalf("expires_at прочитан как %v, записан был %v", stored.UTC(), sent)
	}

	if err := uc.Execute(ctx, command(2, 1<<30, expiresAt)); err != nil {
		t.Fatalf("повторный Execute: %v", err)
	}

	if got := scalar[int64](t, pool, `SELECT count(*) FROM agent_operations`); got != 1 {
		t.Fatalf("операций %d, повтор не должен создавать новых", got)
	}
	if got := scalar[int64](t, pool, `SELECT count(*) FROM quota_periods`); got != 1 {
		t.Fatalf("периодов %d, повтор не должен открывать новый", got)
	}
	if got := scalar[string](t, pool,
		`SELECT quota_period_id::text FROM quota_periods`); got != periodID {
		t.Fatal("период подменён: накопленный трафик был бы сброшен")
	}
	if got := scalar[int64](t, pool,
		`SELECT last_command_number FROM customer_entitlements WHERE customer_id = $1`,
		testCustomerID); got != 2 {
		t.Fatalf("last_command_number = %d, валидный no-op обязан его двигать", got)
	}
	// desired_revision не двигается: состав desired-юзеров ноды не изменился.
	if got := scalar[int64](t, pool,
		`SELECT desired_revision FROM vpn_nodes WHERE node_id = 'node-a'`); got != 2 {
		t.Fatalf("desired_revision = %d, пустой план ноду не трогает", got)
	}
}

// Устаревшая команда завершается идемпотентным OK без единого side effect.
func TestIntegrationApplyStaleCommand(t *testing.T) {
	uc, pool := newFixture(t)
	seedTopology(t, pool, []string{"node-a"}, nil)

	ctx := context.Background()
	expiresAt := time.Now().UTC().Add(30 * 24 * time.Hour)

	if err := uc.Execute(ctx, command(5, 1<<30, expiresAt)); err != nil {
		t.Fatalf("первый Execute: %v", err)
	}

	// Меньший номер с другой квотой: квота примениться не должна.
	if err := uc.Execute(ctx, command(3, 9<<30, expiresAt)); err != nil {
		t.Fatalf("устаревшая команда вернула ошибку: %v", err)
	}

	if got := scalar[int64](t, pool,
		`SELECT last_command_number FROM customer_entitlements WHERE customer_id = $1`,
		testCustomerID); got != 5 {
		t.Fatalf("last_command_number = %d, устаревшая команда его двигать не должна", got)
	}
	if got := scalar[int64](t, pool,
		`SELECT usage_quota_bytes FROM quota_periods WHERE customer_id = $1`,
		testCustomerID); got != 1<<30 {
		t.Fatalf("usage_quota_bytes = %d, устаревшая команда квоту менять не должна", got)
	}
}

// Renewal закрывает текущий период и открывает новый тем ЖЕ timestamp: иначе дельта
// трафика не попала бы ни в один период.
func TestIntegrationApplyRenewalStitchesPeriods(t *testing.T) {
	uc, pool := newFixture(t)
	seedTopology(t, pool, []string{"node-a"}, nil)

	ctx := context.Background()
	expiresAt := time.Now().UTC().Add(24 * time.Hour)

	if err := uc.Execute(ctx, command(1, 1<<30, expiresAt)); err != nil {
		t.Fatalf("первый Execute: %v", err)
	}

	// Расход прошлого периода: renewal обязан начать новый с нуля.
	if _, err := pool.Exec(ctx, `UPDATE node_quota_usage SET uplink_bytes = 500`); err != nil {
		t.Fatalf("подготовка расхода: %v", err)
	}

	if err := uc.Execute(ctx, command(2, 1<<30, expiresAt.Add(30*24*time.Hour))); err != nil {
		t.Fatalf("renewal: %v", err)
	}

	if got := scalar[int64](t, pool, `SELECT count(*) FROM quota_periods`); got != 2 {
		t.Fatalf("периодов %d, ожидалось 2", got)
	}
	if !scalar[bool](t, pool, `
		SELECT (SELECT closed_at FROM quota_periods WHERE closed_at IS NOT NULL)
		     = (SELECT started_at FROM quota_periods WHERE closed_at IS NULL)`) {
		t.Fatal("closed_at старого периода не совпал со started_at нового")
	}
	if got := scalar[int64](t, pool,
		`SELECT total_bytes FROM node_quota_usage nqu
		 JOIN quota_periods qp USING (quota_period_id)
		 WHERE qp.closed_at IS NULL`); got != 0 {
		t.Fatalf("расход нового периода = %d, ожидался 0", got)
	}
	// Access уже PRESENT и остаётся им, поэтому новых операций renewal не создаёт:
	// естественная идемпотентность целевого состояния.
	if got := scalar[int64](t, pool, `SELECT count(*) FROM agent_operations`); got != 1 {
		t.Fatalf("операций %d, ожидалась 1", got)
	}
}

// Понижение квоты блокирует только исчерпавшую ноду: квота применяется независимо
// на каждой ноде.
func TestIntegrationApplyQuotaDecreaseBlocksOneNode(t *testing.T) {
	uc, pool := newFixture(t)
	seedTopology(t, pool, []string{"node-a", "node-b"}, nil)

	ctx := context.Background()
	expiresAt := time.Now().UTC().Add(30 * 24 * time.Hour)

	if err := uc.Execute(ctx, command(1, 10<<30, expiresAt)); err != nil {
		t.Fatalf("первый Execute: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE node_quota_usage SET uplink_bytes = $1 WHERE node_id = 'node-a'`,
		int64(5<<30)); err != nil {
		t.Fatalf("подготовка расхода: %v", err)
	}

	// Новый лимит ниже расхода node-a и выше расхода node-b.
	if err := uc.Execute(ctx, command(2, 1<<30, expiresAt)); err != nil {
		t.Fatalf("понижение квоты: %v", err)
	}

	if got := scalar[int64](t, pool,
		`SELECT count(*) FROM node_quota_usage WHERE exhausted_at IS NOT NULL`); got != 1 {
		t.Fatalf("исчерпанных нод %d, ожидалась 1", got)
	}
	if got := scalar[string](t, pool,
		`SELECT desired_state FROM vpn_accesses WHERE entry_node_id = 'node-a'`); got != "ABSENT" {
		t.Fatalf("desired_state node-a = %q, ожидался ABSENT", got)
	}
	if got := scalar[string](t, pool,
		`SELECT desired_state FROM vpn_accesses WHERE entry_node_id = 'node-b'`); got != "PRESENT" {
		t.Fatalf("desired_state node-b = %q, исчерпание на другой ноде на него не влияет", got)
	}

	// Remove создан только для исчерпавшей ноды, версия выросла.
	if got := scalar[int64](t, pool,
		`SELECT count(*) FROM agent_operations
		 WHERE operation_type = 'ENSURE_ABSENT' AND node_id = 'node-a' AND desired_version = 2`); got != 1 {
		t.Fatalf("ENSURE_ABSENT для node-a: %d, ожидалась 1", got)
	}
	if got := scalar[int64](t, pool,
		`SELECT count(*) FROM agent_operations WHERE node_id = 'node-b'`); got != 1 {
		t.Fatalf("операций на node-b %d, ожидалась 1 (только исходный Present)", got)
	}

	// Прежняя неотправленная операция того же access помечена устаревшей.
	if got := scalar[int64](t, pool,
		`SELECT count(*) FROM agent_operations
		 WHERE node_id = 'node-a' AND desired_version = 1 AND status = 'SUPERSEDED'`); got != 1 {
		t.Fatalf("SUPERSEDED операций %d, ожидалась 1", got)
	}
}

// Неизвестный fleet возвращает NOT_FOUND и не оставляет следов.
func TestIntegrationApplyUnknownFleet(t *testing.T) {
	uc, pool := newFixture(t)
	seedTopology(t, pool, []string{"node-a"}, nil)

	cmd := command(1, 1<<30, time.Now().UTC().Add(24*time.Hour))
	cmd.Command.FleetID = testFleetID + 1

	err := uc.Execute(context.Background(), cmd)
	if !errors.Is(err, domain.ErrFleetNotFound) {
		t.Fatalf("Execute = %v, ожидалась ErrFleetNotFound", err)
	}
	if got := scalar[int64](t, pool, `SELECT count(*) FROM customer_entitlements`); got != 0 {
		t.Fatalf("создано %d entitlement, отклонённая команда не должна ничего писать", got)
	}
}

// Верхняя граница uint64 переживает круг через numeric(20,0). Это же проверяет
// нормализацию Exp: 2^64-1 драйвер возвращает не в той же форме, в какой принял.
func TestIntegrationQuotaRoundTripAtUint64Max(t *testing.T) {
	uc, pool := newFixture(t)
	seedTopology(t, pool, []string{"node-a"}, nil)

	ctx := context.Background()
	expiresAt := time.Now().UTC().Add(30 * 24 * time.Hour)

	const maxQuota = ^uint64(0)
	if err := uc.Execute(ctx, command(1, maxQuota, expiresAt)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !scalar[bool](t, pool,
		`SELECT usage_quota_bytes = 18446744073709551615 FROM quota_periods`) {
		t.Fatal("usage_quota_bytes не совпал с 2^64-1")
	}

	// Повтор той же команды обязан остаться no-op: значение прочитано из базы и
	// сравнено с командой, то есть круг numeric -> uint64 отработал точно.
	if err := uc.Execute(ctx, command(2, maxQuota, expiresAt)); err != nil {
		t.Fatalf("повтор: %v", err)
	}
	if got := scalar[int64](t, pool, `SELECT count(*) FROM agent_operations`); got != 1 {
		t.Fatalf("операций %d: повтор с той же квотой не должен ничего менять", got)
	}

	// command_number тоже numeric(20,0): проверяем его на той же границе.
	if err := uc.Execute(ctx, command(maxQuota, maxQuota, expiresAt)); err != nil {
		t.Fatalf("команда с номером 2^64-1: %v", err)
	}
	if !scalar[bool](t, pool,
		`SELECT last_command_number = 18446744073709551615 FROM customer_entitlements`) {
		t.Fatal("last_command_number не совпал с 2^64-1")
	}
}

// TestIntegrationApplyWritesAudit — audit обязателен для customer
// Apply/renewal. Проверяется на настоящей БД, потому что запись проходит через
// сериализацию metadata в jsonb и nullable-колонки адаптера.
func TestIntegrationApplyWritesAudit(t *testing.T) {
	uc, pool := newFixture(t)
	seedTopology(t, pool, []string{"node-a"}, nil)

	expiresAt := time.Now().UTC().Add(30 * 24 * time.Hour)
	if err := uc.Execute(context.Background(), command(1, 1<<30, expiresAt)); err != nil {
		t.Fatalf("ApplyCustomerAccess: %v", err)
	}

	if got := scalar[int64](t, pool,
		`SELECT count(*) FROM audit_events
		 WHERE action = 'CUSTOMER_CREATED' AND target_type = 'CUSTOMER'
		   AND target_id = $1 AND actor_id = $2 AND outcome = 'ACCEPTED'`,
		testCustomerID, testApplyActor); got != 1 {
		t.Fatalf("записей о создании %d, ожидалась 1", got)
	}

	// Продление даёт отдельное действие, а не повтор CUSTOMER_CREATED.
	if err := uc.Execute(context.Background(),
		command(2, 1<<30, expiresAt.Add(30*24*time.Hour))); err != nil {
		t.Fatalf("продление: %v", err)
	}

	if got := scalar[int64](t, pool,
		`SELECT count(*) FROM audit_events WHERE action = 'CUSTOMER_RENEWED' AND target_id = $1`,
		testCustomerID); got != 1 {
		t.Errorf("записей о продлении %d, ожидалась 1", got)
	}

	// Метаданные доехали структурой, а не строкой, и несут только счётчики.
	if got := scalar[bool](t, pool,
		`SELECT sanitized_metadata->>'new_quota_period' = 'true'
		 FROM audit_events WHERE action = 'CUSTOMER_RENEWED'`); !got {
		t.Error("продление не отмечено открытием нового периода квоты")
	}

	// request_id связывает запись с логами того же запроса.
	if got := scalar[int64](t, pool,
		`SELECT count(*) FROM audit_events WHERE request_id IS NULL`); got != 0 {
		t.Errorf("записей без request_id %d, ожидалось 0", got)
	}
}

// TestIntegrationApplyAuditRollsBackWithCommand — журнал ведётся в той
// же транзакции, что и изменение.
//
// Это главное свойство аудита, а не деталь: запись, пережившая откат, утверждала
// бы про изменения, которых в базе нет, — и разбор инцидента пошёл бы по ложному
// следу. Отклонённая команда следа не оставляет, и это осознанный предел v1.
func TestIntegrationApplyAuditRollsBackWithCommand(t *testing.T) {
	uc, pool := newFixture(t)
	seedTopology(t, pool, []string{"node-a"}, nil)

	// Fleet, которого нет в манифесте: команда отклоняется после того, как
	// транзакция уже открыта.
	cmd := command(1, 1<<30, time.Now().UTC().Add(30*24*time.Hour))
	cmd.Command.FleetID = testFleetID + 1

	if err := uc.Execute(context.Background(), cmd); !errors.Is(err, domain.ErrFleetNotFound) {
		t.Fatalf("Execute = %v, ожидалась ErrFleetNotFound", err)
	}

	if got := scalar[int64](t, pool, `SELECT count(*) FROM audit_events`); got != 0 {
		t.Errorf("записей аудита %d, ожидалось 0: откат не унёс журнал", got)
	}
	if got := scalar[int64](t, pool, `SELECT count(*) FROM customer_entitlements`); got != 0 {
		t.Errorf("строк customer %d, ожидалось 0", got)
	}
}

// TestIntegrationApplyStaleCommandWritesNoAudit — устаревшая команда
// коммитится, но side effects не имеет, поэтому и записи о ней
// быть не должно: повтор доставки иначе плодил бы дубликаты одного no-op.
func TestIntegrationApplyStaleCommandWritesNoAudit(t *testing.T) {
	uc, pool := newFixture(t)
	seedTopology(t, pool, []string{"node-a"}, nil)

	expiresAt := time.Now().UTC().Add(30 * 24 * time.Hour)
	if err := uc.Execute(context.Background(), command(5, 1<<30, expiresAt)); err != nil {
		t.Fatalf("ApplyCustomerAccess: %v", err)
	}
	if err := uc.Execute(context.Background(), command(3, 9<<30, expiresAt)); err != nil {
		t.Fatalf("устаревшая команда вернула ошибку: %v", err)
	}

	if got := scalar[int64](t, pool, `SELECT count(*) FROM audit_events`); got != 1 {
		t.Errorf("записей аудита %d, ожидалась 1: устаревшая команда добавила свою", got)
	}
}

// TestIntegrationApplySerializesConcurrentCommands — две команды на одного
// customer не идут одновременно.
//
// Порядок команд задаёт command_number, но сам по себе он ничего не
// сериализует: две транзакции, прочитавшие один и тот же снимок, обе сочли бы
// свой номер актуальным и обе записали бы своё состояние. Сериализацию даёт
// FOR UPDATE на корневой строке, и проверить его можно только конкурентно —
// последовательные вызовы проходят и без него.
func TestIntegrationApplySerializesConcurrentCommands(t *testing.T) {
	apply, pool := newFixture(t)
	seedTopology(t, pool, []string{"NL-1"}, nil)
	ctx := context.Background()

	expiresAt := time.Now().UTC().Add(30 * 24 * time.Hour)
	if err := apply.Execute(ctx, command(1, 1<<30, expiresAt)); err != nil {
		t.Fatalf("первая команда: %v", err)
	}

	// Транзакция-конкурент держит корневую строку и ещё не закоммитилась. Так
	// выглядит вторая команда продукта, дошедшая до того же customer.
	rival, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = rival.Rollback(ctx) }()

	if _, err := rival.Exec(ctx,
		`SELECT customer_id FROM customer_entitlements WHERE customer_id = $1 FOR UPDATE`,
		testCustomerID); err != nil {
		t.Fatalf("подготовка конкурента: %v", err)
	}

	// Пока конкурент держит строку, вторая команда обязана ждать его commit.
	done := make(chan error, 1)
	go func() {
		done <- apply.Execute(ctx, command(2, 2<<30, expiresAt.Add(time.Hour)))
	}()

	select {
	case err := <-done:
		t.Fatalf("вторая команда прошла при заблокированной корневой строке (%v): "+
			"сериализации на customer нет", err)
	case <-time.After(300 * time.Millisecond):
	}

	// Состояние не поехало, пока команда ждёт: запись идёт после блокировки.
	if got := scalar[int64](t, pool,
		`SELECT last_command_number FROM customer_entitlements WHERE customer_id = $1`,
		testCustomerID); got != 1 {
		t.Errorf("last_command_number %d во время ожидания, ожидался 1", got)
	}

	if err := rival.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("вторая команда после снятия блокировки: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("вторая команда не прошла после снятия блокировки")
	}

	if got := scalar[int64](t, pool,
		`SELECT last_command_number FROM customer_entitlements WHERE customer_id = $1`,
		testCustomerID); got != 2 {
		t.Errorf("last_command_number %d, ожидался 2", got)
	}
}
