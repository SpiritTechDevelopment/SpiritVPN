package nodeagent

import (
	"context"
	"fmt"
	"time"

	nodeagentv1 "github.com/RomanRyabinkin/SpiritVPN/internal/gen/spiritvpn/nodeagent/v1"
)

// DefaultPullTimeout — deadline опроса ноды.
//
// Больше DefaultCallTimeout: Ensure отправляет одну команду, а здесь агент
// возвращает до нескольких batch по 5 000 items, и пять секунд на них может
// не хватить на медленном overlay.
const DefaultPullTimeout = 30 * time.Second

// UsageCursor — позиция в спуле агента.
//
// Пара (SpoolID, Sequence) глобально идентифицирует batch и является ключом
// идемпотентности. Новый SQLite на ноде даёт новый SpoolID и начинает Sequence с
// нуля.
type UsageCursor struct {
	SpoolID  string
	Sequence uint64
}

// UserUsage — дельта трафика одного access за интервал опроса.
type UserUsage struct {
	AccountingID  string
	UplinkBytes   uint64
	DownlinkBytes uint64
}

// UsageBatch — пачка дельт под одним sequence.
type UsageBatch struct {
	Cursor UsageCursor
	// CollectedAt — момент сбора на ноде. По нему item сопоставляется с quota
	// period, поэтому это НЕ время получения backend'ом.
	CollectedAt time.Time
	Items       []UserUsage
}

// NodeState — то, что backend забирает у агента в v1.
//
// activity_batches и users сюда не переносятся намеренно: activity в v1 не
// enforce-ится, а инвентарь юзеров принадлежит reconcile. Чего мы не
// запрашиваем, того не подтверждаем и не храним.
type NodeState struct {
	NodeID  string
	Batches []UsageBatch

	// XrayReachable и XrayUptime — liveness ноды для метрик.
	XrayReachable bool
	XrayUptime    time.Duration

	// NeedsBootstrap — локальное состояние агента новое или повреждено, и до
	// ReconcileUsers ему нельзя удалять юзеров. Здесь только прокидывается.
	NeedsBootstrap bool
}

// PullOutcome — исход опроса ноды.
//
// Отдельный тип, а не Outcome: у чтения нет строки в agent_operations, поэтому
// исходы «применено» и «постоянная ошибка» к нему не применимы. Значим только факт
// успеха плюс код и alert для метрик.
type PullOutcome struct {
	State *NodeState
	// Code — стабильный код исхода, тот же словарь, что и у Ensure.
	Code string
	// Message — санитизированная диагностика; секретов не содержит.
	Message string
	Alert   bool
}

// OK сообщает, что состояние получено и с ним можно работать.
func (o PullOutcome) OK() bool { return o.State != nil }

// GetNodeState забирает неподтверждённые usage-batches ноды.
//
// acknowledged передаётся ровно тем, что backend уже durable закоммитил: агент
// удаляет из спула только подтверждённое, поэтому передать больше — значит
// потерять неучтённый трафик.
//
// maxBatches ограничивает ответ. Ноль отдаёт решение агенту, но мы его задаём:
// один шаг воркера обязан оставаться коротким, иначе lease протухнет прямо во
// время обработки.
func (c *Client) GetNodeState(
	ctx context.Context,
	endpoint Endpoint,
	acknowledged UsageCursor,
	maxBatches uint32,
) PullOutcome {
	agent, err := c.connFor(endpoint)
	if err != nil {
		return pullFailure(classifyTransport(err))
	}

	ctx, cancel := context.WithTimeout(ctx, c.pullTimeout)
	defer cancel()

	response, err := nodeagentv1.NewNodeAgentServiceClient(agent.conn).GetNodeState(ctx, &nodeagentv1.GetNodeStateRequest{
		AcknowledgedUsageThrough: &nodeagentv1.UsageCursor{
			SpoolId:  acknowledged.SpoolID,
			Sequence: acknowledged.Sequence,
		},
		MaxUsageBatches: maxBatches,
		// Инвентарь юзеров и activity в v1 не запрашиваются: первое принадлежит
		// reconcile, второе не enforce-ится. Не запрошенное не
		// подтверждается, и агент их не удаляет.
		IncludeUsers: false,
	})
	if err != nil {
		if identityErr := agent.identity.failure(); identityErr != nil {
			return pullFailure(classifyTransport(identityErr))
		}
		return pullFailure(classifyTransport(err))
	}

	state, err := nodeStateFrom(endpoint, response)
	if err != nil {
		// Ответ разобрать не удалось. Ретраить можно — следующий batch может
		// оказаться целым, — но это дефект версии агента, и его надо видеть.
		return PullOutcome{Code: CodeAgentUnknown, Message: err.Error(), Alert: true}
	}

	return PullOutcome{State: state, Code: CodeApplied}
}

// pullFailure переносит классификацию транспорта в исход опроса.
func pullFailure(outcome Outcome) PullOutcome {
	return PullOutcome{Code: outcome.Code, Message: outcome.Message, Alert: outcome.Alert}
}

// nodeStateFrom разбирает ответ агента.
//
// node_id из тела сверяется с тем, за кого его держит backend: запрещено принимать
// node_id из запроса, и здесь та же логика — нода определяется mTLS-идентичностью,
// а расхождение означает, что соединение ведёт не туда, куда мы думаем.
func nodeStateFrom(endpoint Endpoint, response *nodeagentv1.GetNodeStateResponse) (*NodeState, error) {
	if got := response.GetNodeId(); got != endpoint.NodeID {
		return nil, fmt.Errorf("агент представился как %q, ожидалась нода %q", got, endpoint.NodeID)
	}

	state := &NodeState{
		NodeID:         endpoint.NodeID,
		XrayReachable:  response.GetXray().GetReachable(),
		XrayUptime:     time.Duration(response.GetXray().GetUptimeSeconds()) * time.Second,
		NeedsBootstrap: response.GetNeedsBootstrap(),
	}

	for _, batch := range response.GetUsageBatches() {
		converted, err := usageBatchFrom(batch)
		if err != nil {
			return nil, err
		}
		state.Batches = append(state.Batches, converted)
	}

	return state, nil
}

func usageBatchFrom(batch *nodeagentv1.UsageBatch) (UsageBatch, error) {
	// Пустой spool_id сделал бы ключ идемпотентности неотличимым между спулами, а
	// нулевой sequence не бывает у выданного batch: агент нумерует с единицы.
	spoolID := batch.GetCursor().GetSpoolId()
	if spoolID == "" {
		return UsageBatch{}, fmt.Errorf("usage batch без spool_id")
	}
	if batch.GetCursor().GetSequence() == 0 {
		return UsageBatch{}, fmt.Errorf("usage batch spool %s с нулевым sequence", spoolID)
	}

	converted := UsageBatch{
		Cursor: UsageCursor{
			SpoolID:  spoolID,
			Sequence: batch.GetCursor().GetSequence(),
		},
		CollectedAt: time.UnixMilli(batch.GetCollectedAtUnixMs()).UTC(),
		Items:       make([]UserUsage, 0, len(batch.GetItems())),
	}

	for _, item := range batch.GetItems() {
		converted.Items = append(converted.Items, UserUsage{
			AccountingID:  item.GetAccountingId(),
			UplinkBytes:   item.GetUplinkBytes(),
			DownlinkBytes: item.GetDownlinkBytes(),
		})
	}

	return converted, nil
}
