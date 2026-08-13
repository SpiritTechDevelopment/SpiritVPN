package nodeagent

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	nodeagentv1 "github.com/RomanRyabinkin/SpiritVPN/internal/gen/spiritvpn/nodeagent/v1"
)

// Наблюдение фактического инвентаря Xray.

const observedUUID = "11111111-1111-4111-8111-111111111111"

// observedEndpoint — endpoint, годный для настоящего mTLS. pulledEndpoint для
// этого не подходит: без tls_server_name вызов отвергается ДО набора номера, и
// тест на классификацию транспорта проверял бы разбор конфига.
func observedEndpoint(address string) Endpoint {
	return Endpoint{
		NodeID:              pulledNodeID,
		Address:             address,
		TLSServerName:       testAgentDNSName,
		CertificateIdentity: testAgentIdentity,
	}
}

func pbActualUser(accountingID, credential string, managed bool) *nodeagentv1.ActualUser {
	return &nodeagentv1.ActualUser{
		User: &nodeagentv1.User{
			AccountingId:   accountingID,
			CredentialUuid: credential,
			Flow:           "xtls-rprx-vision",
			EgressKey:      "de-exit",
		},
		BackendManaged: managed,
	}
}

func TestInventoryFromMapsUsers(t *testing.T) {
	observedAt := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)

	inventory := inventoryFrom(&nodeagentv1.GetNodeStateResponse{
		NodeId:                pulledNodeID,
		UsersComplete:         true,
		UsersObservedAtUnixMs: observedAt.UnixMilli(),
		Users: []*nodeagentv1.ActualUser{
			pbActualUser("u.aaa", observedUUID, true),
			pbActualUser("infra.probe", observedUUID, false),
		},
	})

	if !inventory.Complete {
		t.Error("флаг полноты снимка потерян")
	}
	if !inventory.ObservedAt.Equal(observedAt) {
		t.Errorf("момент наблюдения %v, ожидался %v", inventory.ObservedAt, observedAt)
	}
	if got := len(inventory.Users); got != 2 {
		t.Fatalf("юзеров %d, ожидалось 2: чужие отсеиваются сверкой, а не разбором", got)
	}

	first := inventory.Users[0]
	if first.AccountingID != "u.aaa" || !first.BackendManaged {
		t.Errorf("первый юзер разобран как %+v", first)
	}
	if first.ClientUUID.Reveal() != uuid.MustParse(observedUUID) {
		t.Error("credential не доехал до сравнимого вида")
	}
	if first.Flow != "xtls-rprx-vision" || first.EgressKey != "de-exit" {
		t.Errorf("flow %q, egress %q", first.Flow, first.EgressKey)
	}
	if inventory.Users[1].BackendManaged {
		t.Error("чужой юзер помечен как backend-owned")
	}
}

// TestInventoryFromKeepsBrokenCredentialComparable — испорченный uuid не роняет
// разбор всего снимка и не исчезает из него.
//
// Такая запись становится нулевым uuid, то есть заведомым расхождением, — и это
// правда: юзер с нечитаемым credential не работает. Пропустить его молча значило
// бы объявить сломанную ноду совпадающей с desired state.
func TestInventoryFromKeepsBrokenCredentialComparable(t *testing.T) {
	inventory := inventoryFrom(&nodeagentv1.GetNodeStateResponse{
		NodeId:                pulledNodeID,
		UsersComplete:         true,
		UsersObservedAtUnixMs: time.Now().UnixMilli(),
		Users:                 []*nodeagentv1.ActualUser{pbActualUser("u.aaa", "не-uuid-вовсе", true)},
	})

	if got := len(inventory.Users); got != 1 {
		t.Fatalf("юзеров %d, ожидался 1: запись с битым credential выброшена", got)
	}
	if !inventory.Users[0].ClientUUID.IsZero() {
		t.Error("неразбираемый credential не обнулён, и сравнение сочло бы его валидным")
	}
}

// TestInventoryFromWithoutObservation — снимка ещё не делали.
//
// Ноль в users_observed_at_unix_ms по контракту означает «наблюдения нет», а не
// «наблюдение сделано в 1970 году»: превращать его в дату значило бы вечно
// считать такой снимок протухшим вместо «его просто не существует».
func TestInventoryFromWithoutObservation(t *testing.T) {
	inventory := inventoryFrom(&nodeagentv1.GetNodeStateResponse{NodeId: pulledNodeID})

	if !inventory.ObservedAt.IsZero() {
		t.Errorf("момент наблюдения %v, ожидался нулевой", inventory.ObservedAt)
	}
	if inventory.Complete {
		t.Error("снимок без наблюдения объявлен полным")
	}
}

// TestObserveUsersRequestsInventory — include_users обязан уехать взведённым,
// иначе агент вернёт пустой список, и сверка не найдёт расхождений никогда.
//
// Заодно проверяется, что чужой курсор не подтверждается: спул принадлежит
// usage-воркеру, и подтверждение отсюда разрешило бы агенту удалить batch'и,
// которых backend ещё не видел.
func TestObserveUsersRequestsInventory(t *testing.T) {
	ca := newTestCA(t)
	client, err := New(ca.issueBackendFiles(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	agent := &fakeAgent{state: &nodeagentv1.GetNodeStateResponse{
		NodeId:                pulledNodeID,
		UsersComplete:         true,
		UsersObservedAtUnixMs: time.Now().UnixMilli(),
		Users:                 []*nodeagentv1.ActualUser{pbActualUser("u.aaa", observedUUID, true)},
	}}
	address := startAgent(t, ca, agent, []string{testAgentDNSName}, []string{testAgentIdentity})

	endpoint := observedEndpoint(address)

	outcome := client.ObserveUsers(context.Background(), endpoint)
	if !outcome.OK() {
		t.Fatalf("наблюдение не удалось: %s %s", outcome.Code, outcome.Message)
	}

	if !agent.stateReq.GetIncludeUsers() {
		t.Error("include_users не взведён: агент вернёт пустой инвентарь")
	}
	if got := agent.stateReq.GetAcknowledgedUsageThrough().GetSpoolId(); got != "" {
		t.Errorf("подтверждён чужой спул %q", got)
	}
	if got := len(outcome.Inventory.Users); got != 1 {
		t.Errorf("юзеров в инвентаре %d, ожидался 1", got)
	}
}

// TestObserveUsersRejectsForeignNode — инвентарь чужой ноды опаснее его
// отсутствия: сверка нашла бы расхождение по всему набору и запустила полный
// reconcile, который привёл бы ноду к ЧУЖОМУ desired state.
func TestObserveUsersRejectsForeignNode(t *testing.T) {
	ca := newTestCA(t)
	client, err := New(ca.issueBackendFiles(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	agent := &fakeAgent{state: &nodeagentv1.GetNodeStateResponse{NodeId: "чужая-нода"}}
	address := startAgent(t, ca, agent, []string{testAgentDNSName}, []string{testAgentIdentity})

	endpoint := observedEndpoint(address)

	outcome := client.ObserveUsers(context.Background(), endpoint)
	if outcome.OK() {
		t.Fatal("инвентарь чужой ноды принят")
	}
	if !outcome.Alert {
		t.Error("представившийся чужой нодой агент не поднял alert")
	}
}

// TestObserveUsersClassifiesTransportFailure — недоступность ноды остаётся
// недоступностью, а не пустым инвентарём. Пустой инвентарь означал бы, что на
// ноде нет ни одного юзера, и сверка сочла бы это расхождением по всему набору.
func TestObserveUsersClassifiesTransportFailure(t *testing.T) {
	ca := newTestCA(t)
	client, err := New(ca.issueBackendFiles(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	endpoint := observedEndpoint("127.0.0.1:1")

	outcome := client.ObserveUsers(context.Background(), endpoint)
	if outcome.OK() {
		t.Fatal("недоступный агент вернул инвентарь")
	}
	if outcome.Code != CodeUnavailable {
		t.Errorf("код %s, ожидался %s", outcome.Code, CodeUnavailable)
	}
}

// TestObserveUsersRejectsIncompleteEndpoint — непригодный agent_config отсекается
// до набора номера и остаётся retryable: чинится он следующим
// манифестом, поэтому permanent означал бы, что чинить уже нечего.
func TestObserveUsersRejectsIncompleteEndpoint(t *testing.T) {
	agent := &fakeAgent{}
	client, endpoint := newHarness(t, agent)
	endpoint.CertificateIdentity = ""

	outcome := client.ObserveUsers(context.Background(), endpoint)

	if outcome.OK() {
		t.Fatal("наблюдение по непригодному endpoint вернуло инвентарь")
	}
	if outcome.Code != CodeNodeConfigInvalid {
		t.Errorf("код %s, ожидался %s", outcome.Code, CodeNodeConfigInvalid)
	}
	if !outcome.Alert {
		t.Error("испорченный agent_config не поднял alert")
	}
	if agent.calls != 0 {
		t.Errorf("агент вызван %d раз, хотя endpoint отвергнут до набора номера", agent.calls)
	}
}

// TestObserveUsersIdentityMismatchIsPermanent — подмена ноды остаётся security
// failure и на пути наблюдения.
//
// Проверяется отдельно от GetNodeState, потому что приоритет причин каждый вызов
// задаёт сам: gRPC не сохраняет цепочку ошибок рукопожатия, и результат сверки
// идентичности приходится спрашивать у структуры соединения. Забыв этот шаг,
// наблюдение вернуло бы обычный UNAVAILABLE, и подмена ноды выглядела бы сбоем
// сети — то есть поводом просто повторить попытку.
func TestObserveUsersIdentityMismatchIsPermanent(t *testing.T) {
	ca := newTestCA(t)
	client, err := New(ca.issueBackendFiles(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// Агент предъявляет сертификат чужой ноды.
	agent := &fakeAgent{state: &nodeagentv1.GetNodeStateResponse{NodeId: pulledNodeID}}
	address := startAgent(t, ca, agent,
		[]string{testAgentDNSName}, []string{"spiffe://spiritvpn/node/DE-1"})

	outcome := client.ObserveUsers(context.Background(), observedEndpoint(address))

	if outcome.OK() {
		t.Fatal("наблюдение подменённой ноды вернуло инвентарь")
	}
	if outcome.Code != CodeIdentityMismatch {
		t.Errorf("код %q, ожидался %q", outcome.Code, CodeIdentityMismatch)
	}
	if !outcome.Alert {
		t.Error("подмена идентичности не подняла alert: это security failure")
	}
	if agent.calls != 0 {
		t.Errorf("агент получил %d вызовов, хотя идентичность не сошлась", agent.calls)
	}
}
