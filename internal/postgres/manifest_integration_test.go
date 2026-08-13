package postgres

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
)

// Интеграционные тесты приёма манифеста. Смысл этого слоя в SQL: апсерты,
// пометки current = false вместо удаления, частичный уникальный индекс на пару
// (entry, exit) и то, что джоба материализации ставится ровно один раз.

var _ app.ManifestRepository = (*Repository)(nil)

func newManifestFixture(t *testing.T) (*app.ApplyFleetManifest, *pgxpool.Pool) {
	t.Helper()

	_, pool := newFixture(t)
	return app.NewApplyFleetManifest(New(pool)), pool
}

func manifestTestNode(id string) domain.ManifestNode {
	return domain.ManifestNode{
		NodeID: domain.NodeID(id),
		Agent: domain.NodeAgent{
			Endpoint:            "10.0.0.11:9443",
			TLSServerName:       id + ".agent.internal",
			CertificateIdentity: "spiffe://spiritvpn/node/" + id,
		},
		Public: domain.NodePublic{
			Address:          id + ".example.com",
			Port:             443,
			RealityPublicKey: "pub-key",
			ServerName:       "www.example.org",
			ShortID:          "ab12",
			Fingerprint:      "chrome",
			Transport:        domain.TransportTCP,
			Flow:             domain.FlowXTLSRprxVision,
			DisplayName:      "имя " + id,
		},
	}
}

// manifestFixture — два узла, один fleet, одна связь.
func manifestFixture(revision int64) domain.ManifestSnapshot {
	return domain.ManifestSnapshot{
		SchemaVersion: domain.ManifestSchemaVersion,
		Revision:      revision,
		Nodes: []domain.ManifestNode{
			manifestTestNode("NL-1"),
			manifestTestNode("DE-1"),
		},
		Fleets: []domain.ManifestFleet{{
			FleetID: testFleetID,
			NodeIDs: []domain.NodeID{"NL-1", "DE-1"},
			Bridges: []domain.ManifestBridge{{
				RoutingKey:  "nl-1.to-de-1",
				EntryNodeID: "NL-1",
				ExitNodeID:  "DE-1",
				EgressTag:   "de-exit",
				DisplayName: "Netherlands via Germany",
			}},
		}},
	}
}

func applyManifest(
	t *testing.T,
	uc *app.ApplyFleetManifest,
	snapshot domain.ManifestSnapshot,
	allowDestructive bool,
) app.ApplyManifestResult {
	t.Helper()

	result, err := uc.Execute(context.Background(), app.ApplyManifestCommand{
		Snapshot:         snapshot,
		AllowDestructive: allowDestructive,
		Actor:            "infra-ci",
		RequestID:        "req-integration",
	})
	if err != nil {
		t.Fatalf("ApplyFleetManifest: %v", err)
	}
	return result
}

// TestIntegrationManifestProjectsSnapshot — первый приём: журнал, проекция и
// джоба материализации.
func TestIntegrationManifestProjectsSnapshot(t *testing.T) {
	uc, pool := newManifestFixture(t)

	result := applyManifest(t, uc, manifestFixture(7), false)
	if result.Idempotent || result.Revision != 7 {
		t.Fatalf("результат %+v", result)
	}

	if got := scalar[int64](t, pool, `SELECT count(*) FROM manifest_revisions WHERE revision = 7`); got != 1 {
		t.Errorf("строк журнала %d, ожидалась 1", got)
	}
	if got := scalar[int64](t, pool,
		`SELECT count(*) FROM vpn_nodes WHERE current AND manifest_revision = 7`); got != 2 {
		t.Errorf("текущих нод %d, ожидалось 2", got)
	}
	if got := scalar[int64](t, pool, `SELECT count(*) FROM vpn_fleet_nodes WHERE current`); got != 2 {
		t.Errorf("membership %d, ожидалось 2", got)
	}
	if got := scalar[int64](t, pool, `SELECT count(*) FROM vpn_bridge_routes WHERE current`); got != 1 {
		t.Errorf("связей %d, ожидалась 1", got)
	}

	// Джоба ставится в той же транзакции, что и проекция.
	if got := scalar[string](t, pool,
		`SELECT status FROM manifest_materialization_jobs WHERE revision = 7`); got != "PENDING" {
		t.Errorf("статус джобы %q, ожидался PENDING", got)
	}

	// Приём манифеста не двигает desired_revision нод.
	if got := scalar[int64](t, pool,
		`SELECT count(*) FROM vpn_nodes WHERE desired_revision <> 1`); got != 0 {
		t.Errorf("у %d нод сдвинулась desired_revision", got)
	}

	// Раскладка public_config и agent_config.
	if got := scalar[string](t, pool,
		`SELECT public_config->>'display_name' FROM vpn_nodes WHERE node_id = 'NL-1'`); got != "имя NL-1" {
		t.Errorf("display_name %q", got)
	}
	if got := scalar[string](t, pool,
		`SELECT agent_config->>'tls_server_name' FROM vpn_nodes WHERE node_id = 'NL-1'`); got != "NL-1.agent.internal" {
		t.Errorf("tls_server_name %q", got)
	}
	// Канонический payload сохранён без потерь. Сравнение идёт как jsonb, а не
	// побайтово: колонка объявлена как jsonb, и PostgreSQL хранит в ней
	// разобранное значение с пересортированными ключами, а не исходный текст.
	payload, digest := domain.CanonicalizeManifest(manifestFixture(7))
	if got := scalar[bool](t, pool,
		`SELECT canonical_payload = $1::jsonb FROM manifest_revisions WHERE revision = 7`,
		string(payload)); !got {
		t.Error("сохранённый canonical_payload не совпадает с вычисленным")
	}
	if got := scalar[string](t, pool, `SELECT digest FROM manifest_revisions WHERE revision = 7`); got != digest {
		t.Errorf("сохранённый digest %q, ожидался %q", got, digest)
	}
}

// TestIntegrationManifestIdempotentReplay — повтор не пишет ничего,
// включая вторую джобу материализации.
func TestIntegrationManifestIdempotentReplay(t *testing.T) {
	uc, pool := newManifestFixture(t)
	applyManifest(t, uc, manifestFixture(7), false)

	result := applyManifest(t, uc, manifestFixture(7), false)
	if !result.Idempotent {
		t.Fatal("повтор не признан идемпотентным")
	}

	if got := scalar[int64](t, pool, `SELECT count(*) FROM manifest_revisions`); got != 1 {
		t.Errorf("строк журнала %d, ожидалась 1", got)
	}
	if got := scalar[int64](t, pool, `SELECT count(*) FROM manifest_materialization_jobs`); got != 1 {
		t.Errorf("джоб %d, ожидалась 1", got)
	}
}

// TestIntegrationManifestRejects — конфликты с принятым состоянием.
func TestIntegrationManifestRejects(t *testing.T) {
	tests := []struct {
		name     string
		snapshot func() domain.ManifestSnapshot
		want     error
	}{
		{
			name: "та же revision с другим содержимым",
			snapshot: func() domain.ManifestSnapshot {
				s := manifestFixture(7)
				s.Nodes[0].Public.Address = "moved.example.com"
				return s
			},
			want: domain.ErrManifestDigestConflict,
		},
		{
			name:     "более старая revision",
			snapshot: func() domain.ManifestSnapshot { return manifestFixture(6) },
			want:     domain.ErrManifestRevisionRegression,
		},
		{
			name: "пропал ранее принятый fleet",
			snapshot: func() domain.ManifestSnapshot {
				s := manifestFixture(8)
				s.Fleets = nil
				return s
			},
			want: domain.ErrManifestFleetMissing,
		},
		{
			name: "удаление без allow_destructive",
			snapshot: func() domain.ManifestSnapshot {
				s := manifestFixture(8)
				s.Fleets[0].Bridges = nil
				return s
			},
			want: domain.ErrManifestDestructive,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			uc, pool := newManifestFixture(t)
			applyManifest(t, uc, manifestFixture(7), false)

			_, err := uc.Execute(context.Background(), app.ApplyManifestCommand{Snapshot: tc.snapshot()})
			if !errors.Is(err, tc.want) {
				t.Fatalf("ошибка %v, ожидалась %v", err, tc.want)
			}

			// Некорректный манифест не меняет проекцию.
			if got := scalar[int64](t, pool, `SELECT count(*) FROM manifest_revisions`); got != 1 {
				t.Errorf("отклонённый манифест записал журнал: строк %d", got)
			}
			if got := scalar[int64](t, pool, `SELECT count(*) FROM vpn_bridge_routes WHERE current`); got != 1 {
				t.Errorf("отклонённый манифест изменил проекцию: связей %d", got)
			}
		})
	}
}

// TestIntegrationManifestDestructiveMarksInsteadOfDeleting — история не
// удаляется, строки лишь перестают быть текущими. Плюс аудит.
func TestIntegrationManifestDestructiveMarksInsteadOfDeleting(t *testing.T) {
	uc, pool := newManifestFixture(t)
	applyManifest(t, uc, manifestFixture(7), false)

	shrunk := manifestFixture(8)
	shrunk.Nodes = []domain.ManifestNode{manifestTestNode("NL-1")}
	shrunk.Fleets[0].NodeIDs = []domain.NodeID{"NL-1"}
	shrunk.Fleets[0].Bridges = nil

	applyManifest(t, uc, shrunk, true)

	// Строки на месте, но не текущие.
	if got := scalar[int64](t, pool, `SELECT count(*) FROM vpn_nodes`); got != 2 {
		t.Errorf("нод в таблице %d, ожидалось 2 — история не удаляется", got)
	}
	if got := scalar[bool](t, pool, `SELECT current FROM vpn_nodes WHERE node_id = 'DE-1'`); got {
		t.Error("удалённая нода осталась текущей")
	}
	if got := scalar[bool](t, pool,
		`SELECT current FROM vpn_bridge_routes WHERE routing_key = 'nl-1.to-de-1'`); got {
		t.Error("удалённая связь осталась текущей")
	}
	if got := scalar[bool](t, pool,
		`SELECT current FROM vpn_fleet_nodes WHERE node_id = 'DE-1'`); got {
		t.Error("удалённое membership осталось текущим")
	}

	// Manifest_revision удалённой строки остаётся прежней.
	if got := scalar[int64](t, pool,
		`SELECT manifest_revision FROM vpn_nodes WHERE node_id = 'DE-1'`); got != 7 {
		t.Errorf("manifest_revision удалённой ноды %d, ожидалась 7", got)
	}

	// Destructive-манифест обязан оставить запись аудита.
	if got := scalar[int64](t, pool,
		`SELECT count(*) FROM audit_events
		 WHERE action = 'MANIFEST_APPLIED' AND actor_id = 'infra-ci'
		   AND sanitized_metadata->>'destructive' = 'true'`); got != 1 {
		t.Errorf("записей аудита %d, ожидалась 1", got)
	}
}

// TestIntegrationManifestNoAuditWithoutRemovals — обычный приём журнал аудита не
// засоряет (он нужен только для destructive).
func TestIntegrationManifestNoAuditWithoutRemovals(t *testing.T) {
	uc, pool := newManifestFixture(t)
	applyManifest(t, uc, manifestFixture(7), false)

	grown := manifestFixture(8)
	grown.Nodes = append(grown.Nodes, manifestTestNode("FR-1"))
	grown.Fleets[0].NodeIDs = append(grown.Fleets[0].NodeIDs, "FR-1")
	applyManifest(t, uc, grown, false)

	if got := scalar[int64](t, pool, `SELECT count(*) FROM audit_events`); got != 0 {
		t.Fatalf("записей аудита %d, ожидалось 0", got)
	}
}

// TestIntegrationManifestRevivesRow — вернувшаяся нода оживляет свою
// строку, а не заводит новую.
func TestIntegrationManifestRevivesRow(t *testing.T) {
	uc, pool := newManifestFixture(t)
	applyManifest(t, uc, manifestFixture(7), false)

	shrunk := manifestFixture(8)
	shrunk.Nodes = []domain.ManifestNode{manifestTestNode("NL-1")}
	shrunk.Fleets[0].NodeIDs = []domain.NodeID{"NL-1"}
	shrunk.Fleets[0].Bridges = nil
	applyManifest(t, uc, shrunk, true)

	applyManifest(t, uc, manifestFixture(9), false)

	if got := scalar[int64](t, pool, `SELECT count(*) FROM vpn_nodes`); got != 2 {
		t.Errorf("нод в таблице %d, ожидалось 2 — строка обязана ожить на месте", got)
	}
	if got := scalar[int64](t, pool, `SELECT count(*) FROM vpn_nodes WHERE current`); got != 2 {
		t.Errorf("текущих нод %d, ожидалось 2", got)
	}
	if got := scalar[int64](t, pool,
		`SELECT manifest_revision FROM vpn_nodes WHERE node_id = 'DE-1'`); got != 9 {
		t.Errorf("manifest_revision оживлённой ноды %d, ожидалась 9", got)
	}
}

// TestIntegrationManifestRouteTransfer — ради этого правился baseline: пара
// (entry, exit) уникальна только среди ТЕКУЩИХ связей, поэтому перенос route на
// новый routing_key проходит.
func TestIntegrationManifestRouteTransfer(t *testing.T) {
	uc, pool := newManifestFixture(t)
	applyManifest(t, uc, manifestFixture(7), false)

	transferred := manifestFixture(8)
	transferred.Fleets[0].Bridges = []domain.ManifestBridge{{
		RoutingKey:  "nl-1.to-de-1.v2",
		EntryNodeID: "NL-1",
		ExitNodeID:  "DE-1",
		EgressTag:   "de-exit",
		DisplayName: "Netherlands via Germany",
	}}
	applyManifest(t, uc, transferred, true)

	if got := scalar[int64](t, pool, `SELECT count(*) FROM vpn_bridge_routes`); got != 2 {
		t.Errorf("строк связей %d, ожидалось 2 — старая сохраняется", got)
	}
	if got := scalar[string](t, pool,
		`SELECT routing_key FROM vpn_bridge_routes WHERE current`); got != "nl-1.to-de-1.v2" {
		t.Errorf("текущая связь %q", got)
	}
}

// TestIntegrationManifestRepointUpdatesEgressTag — смена egress_tag при
// неизменной паре не destructive и меняет строку на месте.
func TestIntegrationManifestRepointUpdatesEgressTag(t *testing.T) {
	uc, pool := newManifestFixture(t)
	applyManifest(t, uc, manifestFixture(7), false)

	repointed := manifestFixture(8)
	repointed.Fleets[0].Bridges[0].EgressTag = "de-exit-v2"
	applyManifest(t, uc, repointed, false)

	if got := scalar[string](t, pool,
		`SELECT egress_tag FROM vpn_bridge_routes WHERE routing_key = 'nl-1.to-de-1'`); got != "de-exit-v2" {
		t.Errorf("egress_tag %q, ожидался de-exit-v2", got)
	}
	if got := scalar[int64](t, pool, `SELECT count(*) FROM vpn_bridge_routes`); got != 1 {
		t.Errorf("строк связей %d, ожидалась 1", got)
	}
}

// TestIntegrationManifestFeedsCustomerAccess — срез целиком: топология заводится
// манифестом, а не руками, и по ней выдаётся рабочая VLESS URI.
//
// Ровно то, ради чего этот срез шёл первым: до него единственным способом
// положить ноду в базу был INSERT из теста.
func TestIntegrationManifestFeedsCustomerAccess(t *testing.T) {
	applyCustomer, pool := newFixture(t)
	manifestUC := app.NewApplyFleetManifest(New(pool))
	links := app.NewGetCustomerAccessLinks(New(pool), testCipher(t))

	applyManifest(t, manifestUC, manifestFixture(7), false)

	expiresAt := time.Now().UTC().Add(24 * time.Hour)
	if err := applyCustomer.Execute(context.Background(), command(1, 1<<30, expiresAt)); err != nil {
		t.Fatalf("ApplyCustomerAccess: %v", err)
	}

	// apply_state проставляет dispatcher, которого ещё нет.
	exec(t, pool, `UPDATE vpn_accesses SET apply_state = 'APPLIED' WHERE customer_id = $1`, testCustomerID)

	got, err := links.Execute(context.Background(), testCustomerID)
	if err != nil {
		t.Fatalf("GetCustomerAccessLinks: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ссылок %d, ожидалось 3 (2 ноды + 1 связь)", len(got))
	}

	for _, link := range got {
		if link.Status.State != domain.LinkStateReady {
			t.Fatalf("состояние %s, ожидалось READY", link.Status.State)
		}
		parsed, parseErr := url.Parse(link.URI)
		if parseErr != nil {
			t.Fatalf("URI %q не разбирается: %v", link.URI, parseErr)
		}
		if parsed.Query().Get("pbk") != "pub-key" {
			t.Errorf("pbk %q — параметры ноды не доехали из манифеста", parsed.Query().Get("pbk"))
		}
	}

	// Фрагмент FREEDOM берётся из display_name ноды манифеста.
	freedom, _ := url.Parse(got[1].URI)
	if freedom.Fragment != "имя DE-1" && freedom.Fragment != "имя NL-1" {
		t.Errorf("фрагмент %q не из display_name манифеста", freedom.Fragment)
	}
}
