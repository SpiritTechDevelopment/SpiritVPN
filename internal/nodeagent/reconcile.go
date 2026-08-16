package nodeagent

import (
	"context"
	"time"

	nodeagentv1 "github.com/RomanRyabinkin/SpiritVPN/internal/gen/spiritvpn/nodeagent/v1"
)

// DefaultReconcileTimeout — deadline полного набора.
//
// Заметно больше DefaultCallTimeout, потому что это другой по размеру вызов:
// Ensure несёт одного юзера, reconcile — до 2000, и агент применяет их к
// Xray по одному, добавляя каждому ещё и routing rule. Общий deadline на пять
// секунд обрывал бы применение посередине тем чаще, чем крупнее нода.
const DefaultReconcileTimeout = 60 * time.Second

// ReconcileResult — исход полного набора вместе со сводкой изменений.
//
// Счётчики отдельно от Outcome, потому что они не про доставку: агент сообщает,
// сколько юзеров он добавил, заменил, удалил и оставил без изменений. Ненулевой
// removed на живой ноде — это ровно тот дрейф, ради которого reconcile и нужен,
// и без него расхождение чинилось бы молча.
type ReconcileResult struct {
	Outcome

	Added     uint32
	Replaced  uint32
	Removed   uint32
	Unchanged uint32
}

// ReconcileUsers отдаёт ноде полный набор backend-owned юзеров.
//
// Расхождение с исходным замыслом лежит в контракте, а не в поведении: он
// предполагал запрос с desired_revision и ответ с applied_revision, а
// вендоренный node_agent.proto не передаёт ревизию ни туда, ни обратно — вместо
// неё operation_id и обязательный complete. Ревизия остаётся целиком на стороне
// backend, и сверка «desired state не уехал, пока шёл вызов» выполняется
// guarded-UPDATE'ом там же. Контракт агента принадлежит другому репозиторию и
// имеет приоритет.
//
// complete=true — не украшение, а утверждение: агент удаляет всех
// backend-owned юзеров, которых нет в наборе, и поле объявляет, что набор
// авторитетный, а не усечённый по дороге.
func (c *Client) ReconcileUsers(
	ctx context.Context,
	endpoint Endpoint,
	operationID string,
	users []User,
) ReconcileResult {
	agent, err := c.connFor(endpoint)
	if err != nil {
		return ReconcileResult{Outcome: classifyTransport(err)}
	}

	request := &nodeagentv1.ReconcileUsersRequest{
		OperationId: operationID,
		Complete:    true,
		Users:       make([]*nodeagentv1.User, 0, len(users)),
	}
	for _, user := range users {
		request.Users = append(request.Users, &nodeagentv1.User{
			AccountingId: user.AccountingID,
			// Как и в EnsureUserPresent, здесь открытый client_uuid покидает тип и
			// уходит на провод. Payload собирается в памяти и не логируется.
			CredentialUuid: user.ClientUUID.Reveal().String(),
			Flow:           user.Flow,
			EgressKey:      user.EgressKey,
		})
	}

	ctx, cancel := context.WithTimeout(ctx, c.reconcileTimeout)
	defer cancel()

	response, err := nodeagentv1.NewNodeAgentServiceClient(agent.conn).ReconcileUsers(ctx, request)

	return ReconcileResult{
		Outcome:   classifyWithIdentity(agent, response.GetOperation(), err),
		Added:     response.GetAdded(),
		Replaced:  response.GetReplaced(),
		Removed:   response.GetRemoved(),
		Unchanged: response.GetUnchanged(),
	}
}
