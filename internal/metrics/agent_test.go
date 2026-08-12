package metrics

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	"github.com/RomanRyabinkin/SpiritVPN/internal/nodeagent"
)

const (
	testNodeID = "node-a"
	// stepDuration — на столько шагают часы теста при каждом обращении. Вызов
	// спрашивает время дважды, поэтому ровно столько и длится каждый RPC.
	stepDuration = 250 * time.Millisecond
)

// sampleTime — база часов тестов. Фиксированная: момент последнего успеха
// сравнивается точно, а не «примерно сейчас».
var sampleTime = time.Unix(1_700_000_000, 0).UTC()

// fakeAgent — заранее заданные исходы. Настоящего gRPC здесь не поднимается: у
// декоратора нет своей сетевой логики, и поддельный агент проверял бы nodeagent,
// а не метрики (тот же принцип, что и в решении 53).
type fakeAgent struct {
	present   nodeagent.Outcome
	absent    nodeagent.Outcome
	pull      nodeagent.PullOutcome
	reconcile nodeagent.ReconcileResult
}

func (f *fakeAgent) ReconcileUsers(
	context.Context, nodeagent.Endpoint, string, []nodeagent.User,
) nodeagent.ReconcileResult {
	return f.reconcile
}

func (f *fakeAgent) EnsureUserPresent(
	context.Context, nodeagent.Endpoint, string, nodeagent.User,
) nodeagent.Outcome {
	return f.present
}

func (f *fakeAgent) EnsureUserAbsent(
	context.Context, nodeagent.Endpoint, string, string,
) nodeagent.Outcome {
	return f.absent
}

func (f *fakeAgent) GetNodeState(
	context.Context, nodeagent.Endpoint, nodeagent.UsageCursor, uint32,
) nodeagent.PullOutcome {
	return f.pull
}

// wrapAgent оборачивает агента детерминированными часами.
func wrapAgent(t *testing.T, r *Registry, inner Agent) Agent {
	t.Helper()

	observed, ok := r.WrapAgent(inner).(*observedAgent)
	if !ok {
		t.Fatal("WrapAgent вернул не *observedAgent")
	}

	var steps int64
	observed.now = func() time.Time {
		at := sampleTime.Add(time.Duration(steps) * stepDuration)
		steps++
		return at
	}
	return observed
}

func endpoint() nodeagent.Endpoint {
	return nodeagent.Endpoint{NodeID: testNodeID, Address: "10.0.0.1:9443"}
}

// histogramSum — сумма наблюдений семейства. Гистограмму нельзя прочитать через
// testutil.ToFloat64, а проверять надо именно записанную длительность.
func histogramSum(t *testing.T, r *Registry, name string) float64 {
	t.Helper()

	families, err := r.reg.Gather()
	if err != nil {
		t.Fatalf("сбор метрик: %v", err)
	}

	for _, f := range families {
		if f.GetName() != name {
			continue
		}

		var sum float64
		for _, metric := range f.GetMetric() {
			sum += metric.GetHistogram().GetSampleSum()
		}
		return sum
	}

	t.Fatalf("гистограммы %s нет в реестре", name)
	return 0
}

// observeSampleAgentCall наполняет метрики агента одним успешным опросом.
// Используется тестом меток, которому нужны непустые серии.
func observeSampleAgentCall(r *Registry) {
	fake := &fakeAgent{pull: nodeagent.PullOutcome{
		State: &nodeagent.NodeState{NodeID: testNodeID, XrayReachable: true},
		Code:  nodeagent.CodeApplied,
	}}
	r.WrapAgent(fake).GetNodeState(context.Background(), endpoint(), nodeagent.UsageCursor{}, 16)
}

func TestAgentCallRecordsDurationAndOutcome(t *testing.T) {
	registry := New()
	agent := wrapAgent(t, registry, &fakeAgent{
		present: nodeagent.Outcome{Result: domain.AttemptSucceeded, Code: nodeagent.CodeApplied},
	})

	agent.EnsureUserPresent(context.Background(), endpoint(), "op-1", nodeagent.User{})

	if got := histogramSum(t, registry, "spiritvpn_agent_call_duration_seconds"); got != stepDuration.Seconds() {
		t.Errorf("длительность %v, ожидалась %v", got, stepDuration.Seconds())
	}

	calls := registry.agentCalls.WithLabelValues(testNodeID, methodEnsureUserPresent, nodeagent.CodeApplied)
	if got := testutil.ToFloat64(calls); got != 1 {
		t.Errorf("вызовов %v, ожидался 1", got)
	}
}

// TestAgentLastSuccessOnlyOnSuccess — метка последнего успеха обязана стоять на
// месте при отказе. Иначе недоступная нода выглядела бы свежеопрошенной, и
// alert по возрасту последнего успеха не сработал бы никогда — то есть метрика
// врала бы ровно в том случае, ради которого заведена.
func TestAgentLastSuccessOnlyOnSuccess(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome nodeagent.Outcome
		want    float64
	}{
		{
			name:    "успех проставляет момент",
			outcome: nodeagent.Outcome{Result: domain.AttemptSucceeded, Code: nodeagent.CodeApplied},
			want:    float64(sampleTime.Add(stepDuration).Unix()),
		},
		{
			name:    "нода недоступна — момент не двигается",
			outcome: nodeagent.Outcome{Result: domain.AttemptRetryable, Code: nodeagent.CodeUnavailable},
			want:    0,
		},
		{
			name:    "агент ответил отказом — это тоже не успех доставки",
			outcome: nodeagent.Outcome{Result: domain.AttemptPermanent, Code: nodeagent.CodeAgentPermanent},
			want:    0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := New()
			agent := wrapAgent(t, registry, &fakeAgent{absent: tc.outcome})

			agent.EnsureUserAbsent(context.Background(), endpoint(), "op-1", "acc-1")

			got := testutil.ToFloat64(registry.agentLastSuccess.WithLabelValues(testNodeID))
			if got != tc.want {
				t.Errorf("момент последнего успеха %v, ожидался %v", got, tc.want)
			}
		})
	}
}

// TestAgentAlertsCounted закрывает решение 52: alert §9 был временно подменён
// логом уровня ERROR, потому что метрик не существовало.
func TestAgentAlertsCounted(t *testing.T) {
	registry := New()
	agent := wrapAgent(t, registry, &fakeAgent{
		absent: nodeagent.Outcome{
			Result: domain.AttemptPermanent,
			Code:   nodeagent.CodeIdentityMismatch,
			Alert:  true,
		},
	})

	agent.EnsureUserAbsent(context.Background(), endpoint(), "op-1", "acc-1")

	alerts := registry.agentAlerts.WithLabelValues(testNodeID, nodeagent.CodeIdentityMismatch)
	if got := testutil.ToFloat64(alerts); got != 1 {
		t.Errorf("alert-исходов %v, ожидался 1", got)
	}
}

// TestPullPublishesNodeHealth — §15 требует health ноды, и до этого среза
// XrayReachable, XrayUptime и NeedsBootstrap не читал никто.
func TestPullPublishesNodeHealth(t *testing.T) {
	registry := New()
	agent := wrapAgent(t, registry, &fakeAgent{pull: nodeagent.PullOutcome{
		State: &nodeagent.NodeState{
			NodeID:         testNodeID,
			XrayReachable:  true,
			XrayUptime:     90 * time.Second,
			NeedsBootstrap: true,
		},
		Code: nodeagent.CodeApplied,
	}})

	agent.GetNodeState(context.Background(), endpoint(), nodeagent.UsageCursor{}, 16)

	if got := testutil.ToFloat64(registry.nodeXrayReachable.WithLabelValues(testNodeID)); got != 1 {
		t.Errorf("xray_reachable %v, ожидалась 1", got)
	}
	if got := testutil.ToFloat64(registry.nodeXrayUptime.WithLabelValues(testNodeID)); got != 90 {
		t.Errorf("uptime %v, ожидалось 90", got)
	}
	if got := testutil.ToFloat64(registry.nodeNeedsBootstrap.WithLabelValues(testNodeID)); got != 1 {
		t.Errorf("needs_bootstrap %v, ожидалась 1", got)
	}
}

// TestFailedPullLeavesHealthAlone — у отказавшего опроса состояния нет, и писать
// в health-гейджи нечего. Ноль здесь означал бы «Xray недоступен», хотя backend
// просто не дозвонился до агента: это разные аварии с разными адресатами.
func TestFailedPullLeavesHealthAlone(t *testing.T) {
	registry := New()
	agent := wrapAgent(t, registry, &fakeAgent{pull: nodeagent.PullOutcome{
		Code: nodeagent.CodeUnavailable,
	}})

	agent.GetNodeState(context.Background(), endpoint(), nodeagent.UsageCursor{}, 16)

	if got := seriesCount(t, registry, "spiritvpn_node_xray_reachable"); got != 0 {
		t.Errorf("серий xray_reachable %d, ожидалось 0", got)
	}
	if got := testutil.ToFloat64(registry.agentCalls.WithLabelValues(
		testNodeID, methodGetNodeState, nodeagent.CodeUnavailable)); got != 1 {
		t.Errorf("вызовов %v, ожидался 1: отказ обязан считаться", got)
	}
}

// TestUsagePullCappedWhenBatchesAtLimit — единственный доступный backend'у
// признак реального backlog: сколько лежит в спуле, знает только нода.
func TestUsagePullCappedWhenBatchesAtLimit(t *testing.T) {
	const maxBatches = 4

	for _, tc := range []struct {
		name    string
		batches int
		want    float64
	}{
		{name: "упёрлись в потолок", batches: maxBatches, want: 1},
		{name: "спул вычерпан", batches: maxBatches - 1, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := New()
			agent := wrapAgent(t, registry, &fakeAgent{pull: nodeagent.PullOutcome{
				State: &nodeagent.NodeState{
					NodeID:  testNodeID,
					Batches: make([]nodeagent.UsageBatch, tc.batches),
				},
				Code: nodeagent.CodeApplied,
			}})

			agent.GetNodeState(context.Background(), endpoint(), nodeagent.UsageCursor{}, maxBatches)

			got := testutil.ToFloat64(registry.usagePullCapped.WithLabelValues(testNodeID))
			if got != tc.want {
				t.Errorf("отметок потолка %v, ожидалось %v", got, tc.want)
			}
		})
	}
}

// TestAgentPassesOutcomeThrough — декоратор наблюдает, а не решает. Подмена
// исхода здесь означала бы, что диспетчер запишет в agent_operations не то, что
// ответил агент.
func TestAgentPassesOutcomeThrough(t *testing.T) {
	want := nodeagent.Outcome{
		Result:  domain.AttemptRetryable,
		Code:    nodeagent.CodeAgentRetryable,
		Message: "агент занят",
	}

	registry := New()
	agent := wrapAgent(t, registry, &fakeAgent{present: want})

	got := agent.EnsureUserPresent(context.Background(), endpoint(), "op-1", nodeagent.User{})
	if got != want {
		t.Errorf("исход %+v, ожидался %+v", got, want)
	}
}
