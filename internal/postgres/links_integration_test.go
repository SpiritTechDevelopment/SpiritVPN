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

// Интеграционные тесты read-пути. Смысл этого слоя целиком в одном запросе:
// какие access вообще попадают в ответ (не retired, цель в текущем manifest,
// входная нода жива) и откуда берётся exhausted_at текущего периода. Юнит-тест
// маппинга не проверил бы ни один из этих join'ов.
//
// ВАЖНО: apply_state='APPLIED' выставляет operation dispatcher, которого пока не
// существует. Поэтому end-to-end после Apply все ссылки PENDING, а READY тесты
// получают, проставляя apply_state руками — ровно то, что сделает dispatcher.

// Проверка на этапе компиляции: адаптер закрывает и read-порт.
var _ app.LinksRepository = (*Repository)(nil)

func newLinksFixture(t *testing.T) (*app.ApplyCustomerAccess, *app.GetCustomerAccessLinks, *pgxpool.Pool) {
	t.Helper()

	apply, pool := newFixture(t)
	return apply, app.NewGetCustomerAccessLinks(New(pool), testCipher(t)), pool
}

// seedNodePublic заполняет public_config ноды: seedTopology кладёт туда пустой
// объект, а без параметров ноды ссылка не собирается.
func seedNodePublic(t *testing.T, pool *pgxpool.Pool, nodeID, address, displayName string) {
	t.Helper()

	const query = `UPDATE vpn_nodes SET public_config = jsonb_build_object(
	    'address', $2::text,
	    'port', 443,
	    'reality_public_key', 'pub-key',
	    'server_name', 'www.example.org',
	    'short_id', 'ab12',
	    'fingerprint', 'chrome',
	    'transport', 'tcp',
	    'flow', 'xtls-rprx-vision',
	    'display_name', $3::text
	) WHERE node_id = $1`

	if _, err := pool.Exec(context.Background(), query, nodeID, address, displayName); err != nil {
		t.Fatalf("seed public_config %s: %v", nodeID, err)
	}
}

// exec — короткий помощник для правок состояния, которые в v1 делает не Apply, а
// компоненты, которых ещё нет (dispatcher, materialization job).
func exec(t *testing.T, pool *pgxpool.Pool, query string, args ...any) {
	t.Helper()

	if _, err := pool.Exec(context.Background(), query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// dispatchAll имитирует успешную доставку всех операций.
func dispatchAll(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	exec(t, pool, `UPDATE vpn_accesses SET apply_state = 'APPLIED' WHERE customer_id = $1`, testCustomerID)
}

// seedLinksFleet — fleet из двух нод и одной связи, у customer действующий
// доступ. Возвращает готовые use case'ы.
func seedLinksFleet(t *testing.T) (*app.GetCustomerAccessLinks, *pgxpool.Pool) {
	t.Helper()

	apply, links, pool := newLinksFixture(t)
	seedTopology(t, pool, []string{"node-a", "node-b"}, [][4]string{
		{"a-to-b", "node-a", "node-b", "exit-b"},
	})
	seedNodePublic(t, pool, "node-a", "a.example.com", "Netherlands")
	seedNodePublic(t, pool, "node-b", "b.example.com", "Germany")

	expiresAt := time.Now().UTC().Add(30 * 24 * time.Hour)
	if err := apply.Execute(context.Background(), command(1, 1<<30, expiresAt)); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	return links, pool
}

func getLinks(t *testing.T, uc *app.GetCustomerAccessLinks) []app.CustomerAccessLink {
	t.Helper()

	links, err := uc.Execute(context.Background(), testCustomerID)
	if err != nil {
		t.Fatalf("GetCustomerAccessLinks: %v", err)
	}
	return links
}

// TestIntegrationLinksUnknownCustomer — неизвестный customer_id даёт
// NOT_FOUND, и отличить его от customer без ссылок умеет только отдельный запрос
// к корневой строке.
func TestIntegrationLinksUnknownCustomer(t *testing.T) {
	_, links, _ := newLinksFixture(t)

	_, err := links.Execute(context.Background(), "нет такого customer")
	if !errors.Is(err, domain.ErrCustomerNotFound) {
		t.Fatalf("ошибка %v, ожидалась ErrCustomerNotFound", err)
	}
}

// TestIntegrationLinksPendingBeforeDispatch — фактическое состояние системы без
// dispatcher: ссылки существуют, но ни одной URI ещё нет (apply_state обязан
// быть APPLIED).
func TestIntegrationLinksPendingBeforeDispatch(t *testing.T) {
	uc, _ := seedLinksFleet(t)

	links := getLinks(t, uc)
	if len(links) != 3 {
		t.Fatalf("ссылок %d, ожидалось 3 (2 ноды + 1 связь)", len(links))
	}
	for _, link := range links {
		if link.Status.State != domain.LinkStatePending {
			t.Errorf("состояние %s, ожидалось PENDING", link.Status.State)
		}
		if link.URI != "" {
			t.Errorf("URI %q до подтверждения доставки", link.URI)
		}
	}
}

// TestIntegrationLinksReadyAfterDispatch — весь путь целиком: sealed client_uuid
// из базы, параметры ноды из public_config, порядок ответа и правило о
// том, чьё display_name уходит во фрагмент.
func TestIntegrationLinksReadyAfterDispatch(t *testing.T) {
	uc, pool := seedLinksFleet(t)
	dispatchAll(t, pool)

	links := getLinks(t, uc)
	if len(links) != 3 {
		t.Fatalf("ссылок %d, ожидалось 3", len(links))
	}

	// Порядок ответа: (kind, logical_target_key, access_id). BRIDGE впереди FREEDOM.
	wantKinds := []domain.AccessKind{domain.AccessKindBridge, domain.AccessKindFreedom, domain.AccessKindFreedom}
	wantHosts := []string{"a.example.com:443", "a.example.com:443", "b.example.com:443"}
	// Фрагмент BRIDGE — имя связи (seedTopology кладёт туда routing_key),
	// FREEDOM — display_name своей ноды.
	wantFragments := []string{"a-to-b", "Netherlands", "Germany"}

	seenUUIDs := make(map[string]struct{}, len(links))
	for i, link := range links {
		if link.Status.State != domain.LinkStateReady {
			t.Fatalf("ссылка %d в состоянии %s, ожидалось READY", i, link.Status.State)
		}
		if link.Kind != wantKinds[i] {
			t.Errorf("ссылка %d: kind %s, ожидался %s", i, link.Kind, wantKinds[i])
		}

		parsed, err := url.Parse(link.URI)
		if err != nil {
			t.Fatalf("URI %q не разбирается: %v", link.URI, err)
		}
		if parsed.Host != wantHosts[i] {
			t.Errorf("ссылка %d: host %q, ожидался %q", i, parsed.Host, wantHosts[i])
		}
		if parsed.Fragment != wantFragments[i] {
			t.Errorf("ссылка %d: фрагмент %q, ожидался %q", i, parsed.Fragment, wantFragments[i])
		}
		if got := parsed.Query().Get("sni"); got != "www.example.org" {
			t.Errorf("ссылка %d: sni %q, ожидался www.example.org", i, got)
		}

		seenUUIDs[parsed.User.Username()] = struct{}{}
	}

	// У каждого access собственный client_uuid.
	if len(seenUUIDs) != 3 {
		t.Fatalf("различных client_uuid %d, ожидалось 3", len(seenUUIDs))
	}
}

// TestIntegrationLinksBlockedByExpiry — истёкший срок блокирует customer
// целиком, независимо от состояния доставки.
func TestIntegrationLinksBlockedByExpiry(t *testing.T) {
	uc, pool := seedLinksFleet(t)
	dispatchAll(t, pool)
	exec(t, pool, `UPDATE customer_entitlements SET expires_at = now() - interval '1 hour'
	               WHERE customer_id = $1`, testCustomerID)

	for _, link := range getLinks(t, uc) {
		want := domain.LinkStatus{State: domain.LinkStateBlocked, Reason: domain.BlockReasonTimeExpired}
		if link.Status != want {
			t.Errorf("состояние %+v, ожидалось %+v", link.Status, want)
		}
		if link.URI != "" {
			t.Errorf("URI %q у истёкшего customer", link.URI)
		}
	}
}

// TestIntegrationLinksBlockedByQuotaPerNode — квота применяется на каждой
// ноде независимо, поэтому исчерпание на node-a не трогает ссылку на node-b.
//
// FREEDOM node-a и BRIDGE a-to-b обе стоят на node-a, поэтому блокируются вместе:
// трафик всех access customer на одной ноде суммируется в один период.
func TestIntegrationLinksBlockedByQuotaPerNode(t *testing.T) {
	uc, pool := seedLinksFleet(t)
	dispatchAll(t, pool)
	exec(t, pool, `UPDATE node_quota_usage SET exhausted_at = now() WHERE node_id = 'node-a'`)

	links := getLinks(t, uc)
	if len(links) != 3 {
		t.Fatalf("ссылок %d, ожидалось 3", len(links))
	}

	blockedByQuota := domain.LinkStatus{
		State:  domain.LinkStateBlocked,
		Reason: domain.BlockReasonTrafficQuotaExhausted,
	}
	// Порядок ответа: BRIDGE a-to-b, FREEDOM node-a, FREEDOM node-b.
	if links[0].Status != blockedByQuota {
		t.Errorf("BRIDGE на node-a: %+v, ожидалось %+v", links[0].Status, blockedByQuota)
	}
	if links[1].Status != blockedByQuota {
		t.Errorf("FREEDOM node-a: %+v, ожидалось %+v", links[1].Status, blockedByQuota)
	}
	if links[2].Status.State != domain.LinkStateReady || links[2].URI == "" {
		t.Errorf("FREEDOM node-b пострадал от исчерпания на node-a: %+v", links[2])
	}
}

// TestIntegrationLinksExposeQuotaByEntryNode — quota-поля принадлежат входной
// ноде access. Поэтому BRIDGE a-to-b и FREEDOM node-a показывают один расход
// node-a, а FREEDOM node-b — отдельный расход node-b.
func TestIntegrationLinksExposeQuotaByEntryNode(t *testing.T) {
	uc, pool := seedLinksFleet(t)

	exec(t, pool, `UPDATE node_quota_usage
	               SET uplink_bytes = CASE node_id WHEN 'node-a' THEN 100 ELSE 7 END,
	                   downlink_bytes = CASE node_id WHEN 'node-a' THEN 200 ELSE 11 END`)

	links := getLinks(t, uc)
	if len(links) != 3 {
		t.Fatalf("ссылок %d, ожидалось 3", len(links))
	}

	// Порядок: BRIDGE a-to-b, FREEDOM node-a, FREEDOM node-b.
	wantConsumed := []uint64{300, 300, 18}
	for i, link := range links {
		if link.UsageQuotaBytes != 1<<30 {
			t.Errorf("ссылка %d: quota=%d, ожидалось %d", i, link.UsageQuotaBytes, uint64(1<<30))
		}
		if link.ConsumedBytes != wantConsumed[i] {
			t.Errorf("ссылка %d: consumed=%d, ожидалось %d", i, link.ConsumedBytes, wantConsumed[i])
		}
	}
}

// TestIntegrationLinksExpiryBeatsQuota — при одновременно применимых
// причинах наружу уходит TIME_EXPIRED.
func TestIntegrationLinksExpiryBeatsQuota(t *testing.T) {
	uc, pool := seedLinksFleet(t)
	dispatchAll(t, pool)
	exec(t, pool, `UPDATE node_quota_usage SET exhausted_at = now()`)
	exec(t, pool, `UPDATE customer_entitlements SET expires_at = now() - interval '1 hour'
	               WHERE customer_id = $1`, testCustomerID)

	for _, link := range getLinks(t, uc) {
		if link.Status.Reason != domain.BlockReasonTimeExpired {
			t.Errorf("причина %q, ожидалась TIME_EXPIRED", link.Status.Reason)
		}
	}
}

// TestIntegrationLinksExcludesRetired — historical access наружу не отдаётся.
func TestIntegrationLinksExcludesRetired(t *testing.T) {
	uc, pool := seedLinksFleet(t)
	exec(t, pool, `UPDATE vpn_accesses SET retired_at = now()
	               WHERE customer_id = $1 AND kind = 'BRIDGE'`, testCustomerID)

	links := getLinks(t, uc)
	if len(links) != 2 {
		t.Fatalf("ссылок %d, ожидалось 2 после ретайра BRIDGE", len(links))
	}
	for _, link := range links {
		if link.Kind == domain.AccessKindBridge {
			t.Error("ретайрнутый BRIDGE попал в ответ")
		}
	}
}

// TestIntegrationLinksExcludesTargetsMissingFromManifest — //
// Проверяется окно между commit манифеста и отработкой materialization job:
// цель уже исчезла из текущей проекции, а retired_at ещё не проставлен. Такой
// access не должен отдаваться, потому что URI по нему запрещён, а показать
// его нечем.
func TestIntegrationLinksExcludesTargetsMissingFromManifest(t *testing.T) {
	tests := []struct {
		name  string
		drop  string
		count int
	}{
		{
			name:  "связь удалена из fleet",
			drop:  `UPDATE vpn_bridge_routes SET current = false WHERE routing_key = 'a-to-b'`,
			count: 2,
		},
		{
			name:  "нода вышла из fleet",
			drop:  `UPDATE vpn_fleet_nodes SET current = false WHERE node_id = 'node-b'`,
			count: 2,
		},
		{
			// Глобально удалённая нода: backend прекращает выдавать её ссылки.
			// Вместе с FREEDOM уходит и BRIDGE, стоящий на ней как на входной.
			name:  "нода удалена из manifest глобально",
			drop:  `UPDATE vpn_nodes SET current = false WHERE node_id = 'node-a'`,
			count: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			uc, pool := seedLinksFleet(t)
			exec(t, pool, tc.drop)

			if got := len(getLinks(t, uc)); got != tc.count {
				t.Fatalf("ссылок %d, ожидалось %d", got, tc.count)
			}
			// Ни один access не ретайрнут: исключение делает запрос, а не job.
			if got := scalar[int64](t, pool,
				`SELECT count(*) FROM vpn_accesses WHERE retired_at IS NOT NULL`); got != 0 {
				t.Fatalf("ретайрнутых access %d, ожидалось 0", got)
			}
		})
	}
}

// TestIntegrationLinksUnusableNodeConfigDegrades — пустой
// public_config ломает свою ссылку, а не весь ответ.
func TestIntegrationLinksUnusableNodeConfigDegrades(t *testing.T) {
	uc, pool := seedLinksFleet(t)
	dispatchAll(t, pool)
	exec(t, pool, `UPDATE vpn_nodes SET public_config = '{}'::jsonb WHERE node_id = 'node-b'`)

	links := getLinks(t, uc)
	if len(links) != 3 {
		t.Fatalf("ссылок %d, ожидалось 3", len(links))
	}
	// Порядок ответа: BRIDGE a-to-b, FREEDOM node-a, FREEDOM node-b.
	if links[2].Status.State != domain.LinkStateFailed {
		t.Errorf("FREEDOM node-b: %s, ожидалось FAILED", links[2].Status.State)
	}
	for i, link := range links[:2] {
		if link.Status.State != domain.LinkStateReady || link.URI == "" {
			t.Errorf("ссылка %d пострадала от соседней ноды: %+v", i, link)
		}
	}
}

// TestIntegrationLinksEmptyFleet — fleet может временно не содержать нод.
// Пустой список — не NOT_FOUND: customer существует.
func TestIntegrationLinksEmptyFleet(t *testing.T) {
	apply, uc, pool := newLinksFixture(t)
	seedTopology(t, pool, nil, nil)

	expiresAt := time.Now().UTC().Add(24 * time.Hour)
	if err := apply.Execute(context.Background(), command(1, 1<<30, expiresAt)); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := len(getLinks(t, uc)); got != 0 {
		t.Fatalf("ссылок %d, ожидалось 0", got)
	}
}

// TestIntegrationLinksFailedAfterPermanentRejection — исчерпавшая себя доставка
// видна продукту как FAILED, а не как вечный PENDING.
//
// Разница поведенческая: PENDING обещает, что ссылка появится, и продукт вправе
// опрашивать дальше. FAILED говорит, что сама она не появится никогда, и нужен
// новый desired state. До этого теста путь apply_state='FAILED' наружу не
// проверялся ни разу: FAILED в ответе получался только из непригодной ноды.
func TestIntegrationLinksFailedAfterPermanentRejection(t *testing.T) {
	uc, pool := seedLinksFleet(t)

	// Одна нода отвергла операцию навсегда, вторая приняла. Ровно это пишет
	// dispatcher по исходу AttemptPermanent.
	exec(t, pool, `UPDATE vpn_accesses SET apply_state = 'APPLIED' WHERE customer_id = $1`, testCustomerID)
	exec(t, pool,
		`UPDATE vpn_accesses SET apply_state = 'FAILED'
		 WHERE customer_id = $1 AND entry_node_id = 'node-a' AND kind = 'FREEDOM'`, testCustomerID)

	var failed, ready int
	for _, link := range getLinks(t, uc) {
		switch link.Status.State {
		case domain.LinkStateFailed:
			failed++
			if link.URI != "" {
				t.Errorf("FAILED-ссылка несёт URI %q: недоставленный доступ выдан наружу", link.URI)
			}
			// Причина существует только у BLOCKED: у FAILED её в контракте нет.
			if link.Status.Reason != domain.BlockReasonNone {
				t.Errorf("FAILED-ссылка несёт причину %q", link.Status.Reason)
			}
		case domain.LinkStateReady:
			ready++
			if link.URI == "" {
				t.Error("READY-ссылка без URI")
			}
		default:
			t.Errorf("состояние %s, ожидались только FAILED и READY", link.Status.State)
		}
	}

	if failed != 1 {
		t.Errorf("FAILED-ссылок %d, ожидалась 1", failed)
	}
	// Отказ одной ноды не гасит работающие: ответ деградирует по одной ссылке.
	if ready != 2 {
		t.Errorf("READY-ссылок %d, ожидалось 2", ready)
	}
}

// TestIntegrationLinksRetryingStaysPending — доставка, ушедшая на повтор, наружу
// неотличима от ещё не начатой.
//
// Недоступность ноды длится сколько угодно, а число попыток не ограничено, и
// продукт всё это время обязан видеть PENDING: ссылка действительно появится,
// как только нода ответит.
func TestIntegrationLinksRetryingStaysPending(t *testing.T) {
	uc, pool := seedLinksFleet(t)

	exec(t, pool, `UPDATE vpn_accesses SET apply_state = 'APPLIED' WHERE customer_id = $1`, testCustomerID)
	exec(t, pool,
		`UPDATE vpn_accesses SET apply_state = 'RETRYING'
		 WHERE customer_id = $1 AND entry_node_id = 'node-a' AND kind = 'FREEDOM'`, testCustomerID)

	var pending int
	for _, link := range getLinks(t, uc) {
		if link.Status.State == domain.LinkStatePending {
			pending++
			if link.URI != "" {
				t.Errorf("PENDING-ссылка несёт URI %q", link.URI)
			}
		}
	}

	if pending != 1 {
		t.Errorf("PENDING-ссылок %d, ожидалась 1: повтор доставки показан не как PENDING", pending)
	}
}
