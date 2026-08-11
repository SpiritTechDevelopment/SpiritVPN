package metrics

import (
	"context"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	"github.com/RomanRyabinkin/SpiritVPN/internal/nodeagent"
)

// Agent — исходящая поверхность к нодам целиком: оба порта, которые уже
// реализует один *nodeagent.Client.
//
// Объединены в один интерфейс, потому что декоратор обязан реализовать оба сразу:
// composition root подставляет один и тот же объект и диспетчеру, и pull worker'у.
type Agent interface {
	app.AgentDispatcher
	app.UsageAgent
}

// WrapAgent инструментирует вызовы к нодам (§15).
//
// Декоратор, а не изменение nodeagent.Client: так ни один пакет ниже composition
// root не узнаёт про Prometheus, и ни одна сигнатура use case не меняется. Всё,
// что нужно метрикам, уже есть в возвращаемых значениях — стабильный Code, флаг
// Alert и NodeState с health ноды.
func (r *Registry) WrapAgent(inner Agent) Agent {
	return &observedAgent{inner: inner, reg: r, now: time.Now}
}

// observedAgent — Agent, считающий каждый вызов.
//
// now — поле, а не time.Now напрямую: тест длительности иначе был бы либо
// недетерминированным, либо не существовал бы.
type observedAgent struct {
	inner Agent
	reg   *Registry
	now   func() time.Time
}

func (a *observedAgent) EnsureUserPresent(
	ctx context.Context,
	endpoint nodeagent.Endpoint,
	operationID string,
	user nodeagent.User,
) nodeagent.Outcome {
	started := a.now()
	outcome := a.inner.EnsureUserPresent(ctx, endpoint, operationID, user)

	a.observe(endpoint.NodeID, methodEnsureUserPresent, started, outcome.Code, outcome.Alert,
		outcome.Result == domain.AttemptSucceeded)
	return outcome
}

func (a *observedAgent) EnsureUserAbsent(
	ctx context.Context,
	endpoint nodeagent.Endpoint,
	operationID, accountingID string,
) nodeagent.Outcome {
	started := a.now()
	outcome := a.inner.EnsureUserAbsent(ctx, endpoint, operationID, accountingID)

	a.observe(endpoint.NodeID, methodEnsureUserAbsent, started, outcome.Code, outcome.Alert,
		outcome.Result == domain.AttemptSucceeded)
	return outcome
}

// GetNodeState — он же per-node heartbeat.
//
// Ensure* вызываются только когда есть что доставлять, поэтому по ним нельзя
// отличить исправную ноду от той, к которой давно не обращались. Опрос идёт по
// каждой текущей ноде каждый интервал (§12), и именно он задаёт
// agent_last_success_timestamp_seconds для §15.
func (a *observedAgent) GetNodeState(
	ctx context.Context,
	endpoint nodeagent.Endpoint,
	acknowledged nodeagent.UsageCursor,
	maxBatches uint32,
) nodeagent.PullOutcome {
	started := a.now()
	outcome := a.inner.GetNodeState(ctx, endpoint, acknowledged, maxBatches)

	// Успех здесь — «агент ответил разбираемым состоянием», то есть PullOutcome.OK.
	// Другого определения у опроса нет: он ничего не применяет.
	a.observe(endpoint.NodeID, methodGetNodeState, started, outcome.Code, outcome.Alert, outcome.OK())

	if outcome.OK() {
		a.observeNodeHealth(endpoint.NodeID, outcome.State, maxBatches)
	}
	return outcome
}

// observe записывает то, что есть у любого вызова: длительность, исход и —
// при успехе — момент последнего успеха.
func (a *observedAgent) observe(
	nodeID, method string,
	started time.Time,
	code string,
	alert, succeeded bool,
) {
	finished := a.now()

	a.reg.agentCallDuration.WithLabelValues(method, code).Observe(finished.Sub(started).Seconds())
	a.reg.agentCalls.WithLabelValues(nodeID, method, code).Inc()

	if alert {
		a.reg.agentAlerts.WithLabelValues(nodeID, code).Inc()
	}
	if succeeded {
		a.reg.agentLastSuccess.WithLabelValues(nodeID).Set(float64(finished.Unix()))
	}
}

// observeNodeHealth снимает liveness ноды из ответа агента (§15).
//
// До этого среза XrayReachable, XrayUptime и NeedsBootstrap не читал никто: они
// приезжали в NodeState и там же оставались.
func (a *observedAgent) observeNodeHealth(nodeID string, state *nodeagent.NodeState, maxBatches uint32) {
	a.reg.nodeXrayReachable.WithLabelValues(nodeID).Set(boolGauge(state.XrayReachable))
	a.reg.nodeXrayUptime.WithLabelValues(nodeID).Set(state.XrayUptime.Seconds())
	a.reg.nodeNeedsBootstrap.WithLabelValues(nodeID).Set(boolGauge(state.NeedsBootstrap))

	// Агент отдал ровно столько batch, сколько разрешили, — значит в спуле
	// осталось ещё. Это единственный доступный backend'у признак реального
	// backlog: сколько лежит в спуле, знает только нода, и снимок БД показывает
	// лишь возраст последнего опроса.
	if maxBatches > 0 && uint32(len(state.Batches)) >= maxBatches {
		a.reg.usagePullCapped.WithLabelValues(nodeID).Inc()
	}
}
