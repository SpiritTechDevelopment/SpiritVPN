package nodeagent

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nodeagentv1 "github.com/RomanRyabinkin/SpiritVPN/internal/gen/spiritvpn/nodeagent/v1"
)

// Юнит-тесты разбора ответа агента. Смысл слоя — граница доверия: всё, что
// приезжает с чужой стороны, здесь либо превращается в NodeState, либо
// отвергается. Транспорт и классификация исходов покрыты client_test.go.

const pulledNodeID = "node-a"

func pulledEndpoint() Endpoint {
	return Endpoint{NodeID: pulledNodeID, Address: "10.0.0.1:9443"}
}

// pbBatch — корректный batch протокола.
func pbBatch(spoolID string, sequence uint64, collectedAtMs int64) *nodeagentv1.UsageBatch {
	return &nodeagentv1.UsageBatch{
		Cursor:            &nodeagentv1.UsageCursor{SpoolId: spoolID, Sequence: sequence},
		CollectedAtUnixMs: collectedAtMs,
		Items: []*nodeagentv1.UserUsage{
			{AccountingId: "u.testaccountingid00001", UplinkBytes: 100, DownlinkBytes: 200},
		},
	}
}

// TestNodeStateFromRejectsForeignNodeID — §10 запрещает принимать node_id из
// запроса; здесь та же логика для ответа. Нода определяется mTLS-идентичностью, и
// расхождение означает, что соединение ведёт не туда, куда мы думаем.
//
// Это не сверка ради аккуратности: приняв чужой node_id, воркер начислил бы
// трафик и подтвердил спул не той ноде.
func TestNodeStateFromRejectsForeignNodeID(t *testing.T) {
	state, err := nodeStateFrom(pulledEndpoint(), &nodeagentv1.GetNodeStateResponse{
		NodeId: "node-b",
	})

	if err == nil {
		t.Fatal("агент представился чужой нодой, но ответ принят")
	}
	if state != nil {
		t.Error("при расхождении node_id вернулось состояние")
	}
	if !strings.Contains(err.Error(), "node-b") || !strings.Contains(err.Error(), pulledNodeID) {
		t.Errorf("ошибка %q не называет обе стороны расхождения", err)
	}
}

// TestNodeStateFromRejectsEmptyNodeID — пустой node_id это тоже расхождение, а не
// «поле не заполнено»: молча принять его значило бы принимать ответ от кого угодно.
func TestNodeStateFromRejectsEmptyNodeID(t *testing.T) {
	if _, err := nodeStateFrom(pulledEndpoint(), &nodeagentv1.GetNodeStateResponse{}); err == nil {
		t.Fatal("ответ без node_id принят")
	}
}

// TestNodeStateFromMapsHealth — три поля liveness §15. До среза метрик их не
// читал никто, и ошибка перевода прошла бы незамеченной.
func TestNodeStateFromMapsHealth(t *testing.T) {
	state, err := nodeStateFrom(pulledEndpoint(), &nodeagentv1.GetNodeStateResponse{
		NodeId:         pulledNodeID,
		NeedsBootstrap: true,
		Xray:           &nodeagentv1.XrayState{Reachable: true, UptimeSeconds: 90},
	})
	if err != nil {
		t.Fatalf("nodeStateFrom: %v", err)
	}

	if state.NodeID != pulledNodeID {
		t.Errorf("node_id %q, ожидался %q", state.NodeID, pulledNodeID)
	}
	if !state.XrayReachable {
		t.Error("xray_reachable потерян")
	}
	// Секунды протокола против time.Duration: перепутанные единицы дали бы
	// правдоподобное, но неверное значение — 90 наносекунд вместо 90 секунд.
	if state.XrayUptime != 90*time.Second {
		t.Errorf("uptime %v, ожидалось 90s", state.XrayUptime)
	}
	if !state.NeedsBootstrap {
		t.Error("needs_bootstrap потерян")
	}
}

// TestNodeStateFromWithoutXray — секция xray необязательна, и её отсутствие не
// должно валить разбор: недоступный Xray именно так и выглядит.
func TestNodeStateFromWithoutXray(t *testing.T) {
	state, err := nodeStateFrom(pulledEndpoint(), &nodeagentv1.GetNodeStateResponse{
		NodeId: pulledNodeID,
	})
	if err != nil {
		t.Fatalf("nodeStateFrom: %v", err)
	}

	if state.XrayReachable || state.XrayUptime != 0 {
		t.Errorf("без секции xray получено reachable=%v uptime=%v",
			state.XrayReachable, state.XrayUptime)
	}
	if len(state.Batches) != 0 {
		t.Errorf("batch'ей %d, ожидалось 0", len(state.Batches))
	}
}

// TestNodeStateFromMapsBatch — CollectedAt приезжает в миллисекундах и по нему
// item сопоставляется с quota period (§12). Это НЕ время получения backend'ом,
// поэтому перевод обязан быть точным.
func TestNodeStateFromMapsBatch(t *testing.T) {
	collectedAt := time.Date(2026, 8, 10, 11, 59, 0, 0, time.UTC)

	state, err := nodeStateFrom(pulledEndpoint(), &nodeagentv1.GetNodeStateResponse{
		NodeId:       pulledNodeID,
		UsageBatches: []*nodeagentv1.UsageBatch{pbBatch("spool-1", 7, collectedAt.UnixMilli())},
	})
	if err != nil {
		t.Fatalf("nodeStateFrom: %v", err)
	}

	if len(state.Batches) != 1 {
		t.Fatalf("batch'ей %d, ожидался 1", len(state.Batches))
	}

	batch := state.Batches[0]
	if want := (UsageCursor{SpoolID: "spool-1", Sequence: 7}); batch.Cursor != want {
		t.Errorf("курсор %+v, ожидался %+v", batch.Cursor, want)
	}
	if !batch.CollectedAt.Equal(collectedAt) {
		t.Errorf("момент сбора %v, ожидался %v", batch.CollectedAt, collectedAt)
	}
	// UTC, а не local: момент сравнивается с границами периода в PostgreSQL.
	if batch.CollectedAt.Location() != time.UTC {
		t.Errorf("момент сбора в зоне %v, ожидался UTC", batch.CollectedAt.Location())
	}

	if len(batch.Items) != 1 {
		t.Fatalf("items %d, ожидался 1", len(batch.Items))
	}
	want := UserUsage{AccountingID: "u.testaccountingid00001", UplinkBytes: 100, DownlinkBytes: 200}
	if batch.Items[0] != want {
		t.Errorf("item %+v, ожидался %+v", batch.Items[0], want)
	}
}

// TestNodeStateFromRejectsBadCursor — ключ идемпотентности §12 это пара
// (spool_id, sequence). Пустой spool_id сделал бы её неотличимой между спулами, а
// нулевого sequence у выданного batch не бывает: агент нумерует с единицы.
//
// Принять такой batch значило бы начислить трафик с ключом, по которому дедуп
// потом не сработает.
func TestNodeStateFromRejectsBadCursor(t *testing.T) {
	for _, tc := range []struct {
		name  string
		batch *nodeagentv1.UsageBatch
	}{
		{
			name:  "без spool_id",
			batch: pbBatch("", 1, 0),
		},
		{
			name:  "нулевой sequence",
			batch: pbBatch("spool-1", 0, 0),
		},
		{
			name:  "курсора нет вовсе",
			batch: &nodeagentv1.UsageBatch{CollectedAtUnixMs: 0},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, err := nodeStateFrom(pulledEndpoint(), &nodeagentv1.GetNodeStateResponse{
				NodeId:       pulledNodeID,
				UsageBatches: []*nodeagentv1.UsageBatch{tc.batch},
			})

			if err == nil {
				t.Fatal("некорректный batch принят")
			}
			if state != nil {
				t.Error("при некорректном batch вернулось состояние")
			}
		})
	}
}

// TestNodeStateFromRejectsWholeResponseOnBadBatch — один испорченный batch
// отвергает ВЕСЬ ответ, а не пропускается.
//
// Иначе курсор уехал бы за пропущенный batch, агент удалил бы его из спула как
// подтверждённый, и трафик пропал бы безвозвратно. Потерять весь опрос дешевле:
// он повторится через интервал.
func TestNodeStateFromRejectsWholeResponseOnBadBatch(t *testing.T) {
	state, err := nodeStateFrom(pulledEndpoint(), &nodeagentv1.GetNodeStateResponse{
		NodeId: pulledNodeID,
		UsageBatches: []*nodeagentv1.UsageBatch{
			pbBatch("spool-1", 1, 0),
			pbBatch("spool-1", 0, 0),
		},
	})

	if err == nil {
		t.Fatal("ответ с испорченным вторым batch принят")
	}
	if state != nil {
		t.Error("вернулось состояние с частично разобранными batch")
	}
}

// --- вызов целиком, через настоящий mTLS ---------------------------------------

// TestGetNodeStateDeliversCursorAndLimit — агенту уходит ровно то, что backend
// durable закоммитил (§12).
//
// Передать больше подтверждённого — значит разрешить агенту удалить из спула
// неучтённый трафик, и восстановить его будет уже неоткуда.
func TestGetNodeStateDeliversCursorAndLimit(t *testing.T) {
	agent := &fakeAgent{state: &nodeagentv1.GetNodeStateResponse{
		NodeId:         "NL-1",
		NeedsBootstrap: true,
		Xray:           &nodeagentv1.XrayState{Reachable: true, UptimeSeconds: 42},
		UsageBatches:   []*nodeagentv1.UsageBatch{pbBatch("spool-1", 3, 0)},
	}}

	client, endpoint := newHarness(t, agent)
	acknowledged := UsageCursor{SpoolID: "spool-1", Sequence: 2}

	outcome := client.GetNodeState(context.Background(), endpoint, acknowledged, 16)

	if !outcome.OK() {
		t.Fatalf("опрос не удался: код %s, %s", outcome.Code, outcome.Message)
	}
	if outcome.Code != CodeApplied || outcome.Alert {
		t.Errorf("исход успеха: код %s, alert %v", outcome.Code, outcome.Alert)
	}

	if got := agent.stateReq.GetAcknowledgedUsageThrough(); got.GetSpoolId() != acknowledged.SpoolID ||
		got.GetSequence() != acknowledged.Sequence {
		t.Errorf("агенту передана позиция %s/%d, ожидалась %s/%d",
			got.GetSpoolId(), got.GetSequence(), acknowledged.SpoolID, acknowledged.Sequence)
	}
	if got := agent.stateReq.GetMaxUsageBatches(); got != 16 {
		t.Errorf("потолок batch %d, ожидался 16", got)
	}

	// §10 и §12: чего не запрашиваем, того не подтверждаем. Инвентарь юзеров
	// принадлежит reconcile, и запросив его здесь, backend заставил бы агента
	// считать его подтверждённым.
	if agent.stateReq.GetIncludeUsers() {
		t.Error("запрошен инвентарь юзеров, хотя v1 его не обрабатывает")
	}

	if !outcome.State.XrayReachable || outcome.State.XrayUptime != 42*time.Second {
		t.Errorf("health ноды не доехал: reachable=%v uptime=%v",
			outcome.State.XrayReachable, outcome.State.XrayUptime)
	}
	if len(outcome.State.Batches) != 1 {
		t.Errorf("batch'ей %d, ожидался 1", len(outcome.State.Batches))
	}
}

// TestGetNodeStateClassifiesTransportFailure — отказ транспорта получает тот же
// стабильный код, что и у Ensure: словарь исходов один на оба пути (§15).
func TestGetNodeStateClassifiesTransportFailure(t *testing.T) {
	agent := &fakeAgent{stateErr: status.Error(codes.Unavailable, "перезапускаюсь")}
	client, endpoint := newHarness(t, agent)

	outcome := client.GetNodeState(context.Background(), endpoint, UsageCursor{}, 16)

	if outcome.OK() {
		t.Fatal("отказ агента вернул состояние")
	}
	if outcome.Code != CodeUnavailable {
		t.Errorf("код %s, ожидался %s", outcome.Code, CodeUnavailable)
	}
	// Недоступность ноды штатна (§16) и оператора будить не должна.
	if outcome.Alert {
		t.Error("обычная недоступность подняла alert")
	}
}

// TestGetNodeStateAlertsOnUnparsableResponse — ответ разобрать не удалось.
// Ретраить можно, но это дефект версии агента, и его надо видеть.
func TestGetNodeStateAlertsOnUnparsableResponse(t *testing.T) {
	agent := &fakeAgent{state: &nodeagentv1.GetNodeStateResponse{NodeId: "NL-2"}}
	client, endpoint := newHarness(t, agent)

	outcome := client.GetNodeState(context.Background(), endpoint, UsageCursor{}, 16)

	if outcome.OK() {
		t.Fatal("ответ чужой ноды принят как состояние")
	}
	if outcome.Code != CodeAgentUnknown {
		t.Errorf("код %s, ожидался %s", outcome.Code, CodeAgentUnknown)
	}
	if !outcome.Alert {
		t.Error("неразбираемый ответ не поднял alert")
	}
}

// TestGetNodeStateRejectsIncompleteEndpoint — непригодный agent_config отсекается
// до набора номера и остаётся retryable (решение 50): чинится он следующим
// манифестом, поэтому permanent означал бы, что чинить уже нечего.
func TestGetNodeStateRejectsIncompleteEndpoint(t *testing.T) {
	agent := &fakeAgent{}
	client, endpoint := newHarness(t, agent)
	endpoint.CertificateIdentity = ""

	outcome := client.GetNodeState(context.Background(), endpoint, UsageCursor{}, 16)

	if outcome.OK() {
		t.Fatal("опрос по непригодному endpoint вернул состояние")
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

// TestGetNodeStateIdentityMismatchIsPermanent — подмена ноды остаётся security
// failure и на пути опроса, а не только у Ensure.
//
// Проверяется отдельно, потому что приоритет причин задан в GetNodeState своим
// кодом: gRPC не сохраняет цепочку ошибок рукопожатия, и результат сверки
// приходится спрашивать у структуры соединения. Забыв этот шаг, опрос вернул бы
// обычный UNAVAILABLE, и подмена ноды выглядела бы как сбой сети.
func TestGetNodeStateIdentityMismatchIsPermanent(t *testing.T) {
	ca := newTestCA(t)
	client, err := New(ca.issueBackendFiles(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	agent := &fakeAgent{state: &nodeagentv1.GetNodeStateResponse{NodeId: "NL-1"}}
	address := startAgent(t, ca, agent,
		[]string{testAgentDNSName}, []string{"spiffe://spiritvpn/node/DE-1"})

	outcome := client.GetNodeState(context.Background(), Endpoint{
		NodeID:              "NL-1",
		Address:             address,
		TLSServerName:       testAgentDNSName,
		CertificateIdentity: testAgentIdentity,
	}, UsageCursor{}, 16)

	if outcome.OK() {
		t.Fatal("опрос подменённой ноды вернул состояние")
	}
	if outcome.Code != CodeIdentityMismatch {
		t.Errorf("код %q, ожидался %q", outcome.Code, CodeIdentityMismatch)
	}
	if !outcome.Alert {
		t.Error("подмена идентичности не подняла alert (§9: security failure)")
	}
	if agent.calls != 0 {
		t.Errorf("агент получил %d вызовов: спул подтверждён чужой ноде", agent.calls)
	}
}
