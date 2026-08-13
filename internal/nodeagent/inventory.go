package nodeagent

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	nodeagentv1 "github.com/RomanRyabinkin/SpiritVPN/internal/gen/spiritvpn/nodeagent/v1"
)

// ActualUser — юзер, наблюдённый агентом непосредственно в Xray.
//
// BackendManaged отделяет наш namespace от чужого. Агент не удаляет
// инфраструктурных и неизвестных юзеров даже при complete-наборе, поэтому и
// сверка обязана их не замечать: иначе каждый чужой юзер выглядел бы вечным
// расхождением, которое ничем не чинится.
type ActualUser struct {
	User

	BackendManaged bool
}

// Inventory — фактическое состояние ноды на момент наблюдения.
type Inventory struct {
	Users []ActualUser

	// ObservedAt — момент наблюдения НА ноде, а не получения ответа. Нулевой
	// означает, что полного наблюдения у агента не существует.
	ObservedAt time.Time

	// Complete — список не усечён. Пока он false, набор нельзя считать
	// авторитетным ни в одну сторону.
	Complete bool
}

// InventoryOutcome — исход наблюдения.
//
// Отдельный тип по той же причине, что и PullOutcome: у чтения нет строки в
// agent_operations, и исходы доставки к нему не применимы.
type InventoryOutcome struct {
	Inventory *Inventory

	Code    string
	Message string
	Alert   bool
}

// OK сообщает, что ответ разобран. Пригодность инвентаря для сверки — вопрос
// отдельный и решается уже в домене.
func (o InventoryOutcome) OK() bool { return o.Inventory != nil }

// ObserveUsers запрашивает фактический инвентарь ноды.
//
// Это тот же GetNodeState, что и у учёта, только с include_users. Отдельного RPC
// под инвентарь в контракте нет, и последствий у совмещения два.
//
// Первое: ответ придёт вместе с usage-batches, которых мы не просили. Отключить
// их нельзя — max_usage_batches=0 означает «на усмотрение агента», а не «ни
// одного». Мы их игнорируем и НЕ подтверждаем: неподтверждённое агент не
// удаляет, и те же batch'и штатно приедут к usage-воркеру.
//
// Второе: acknowledged_usage_through уходит пустым. Курсор принадлежит
// usage-воркеру, и подтверждение чужой позиции здесь разрешило бы агенту
// удалить то, что backend ещё не закоммитил.
func (c *Client) ObserveUsers(ctx context.Context, endpoint Endpoint) InventoryOutcome {
	agent, err := c.connFor(endpoint)
	if err != nil {
		return inventoryFailure(classifyTransport(err))
	}

	ctx, cancel := context.WithTimeout(ctx, c.pullTimeout)
	defer cancel()

	response, err := nodeagentv1.NewNodeAgentServiceClient(agent.conn).GetNodeState(
		ctx, &nodeagentv1.GetNodeStateRequest{IncludeUsers: true})
	if err != nil {
		if identityErr := agent.identity.failure(); identityErr != nil {
			return inventoryFailure(classifyTransport(identityErr))
		}
		return inventoryFailure(classifyTransport(err))
	}

	if got := response.GetNodeId(); got != endpoint.NodeID {
		// Инвентарь чужой ноды опаснее отсутствия инвентаря: сверка нашла бы
		// расхождение по всему набору и запустила полный reconcile, который
		// привёл бы ноду к чужому desired state.
		return InventoryOutcome{
			Code:    CodeAgentUnknown,
			Message: "агент представился чужой нодой",
			Alert:   true,
		}
	}

	return InventoryOutcome{Inventory: inventoryFrom(response), Code: CodeApplied}
}

// inventoryFrom переносит инвентарь из ответа.
func inventoryFrom(response *nodeagentv1.GetNodeStateResponse) *Inventory {
	inventory := &Inventory{
		Complete: response.GetUsersComplete(),
		Users:    make([]ActualUser, 0, len(response.GetUsers())),
	}

	if millis := response.GetUsersObservedAtUnixMs(); millis > 0 {
		inventory.ObservedAt = time.UnixMilli(millis).UTC()
	}

	for _, actual := range response.GetUsers() {
		inventory.Users = append(inventory.Users, ActualUser{
			User: User{
				AccountingID: actual.GetUser().GetAccountingId(),
				// Неразбираемый uuid остаётся нулевым намеренно. Такая запись не
				// совпадёт ни с одним desired и станет расхождением — что и есть
				// правда: юзер с испорченным credential не работает, и чинить его
				// надо, а не пропускать молча.
				ClientUUID: crypto.NewClientUUID(parseUUID(actual.GetUser().GetCredentialUuid())),
				Flow:       actual.GetUser().GetFlow(),
				EgressKey:  actual.GetUser().GetEgressKey(),
			},
			BackendManaged: actual.GetBackendManaged(),
		})
	}

	return inventory
}

func parseUUID(value string) uuid.UUID {
	parsed, err := uuid.Parse(value)
	if err != nil {
		return uuid.Nil
	}
	return parsed
}

func inventoryFailure(outcome Outcome) InventoryOutcome {
	return InventoryOutcome{Code: outcome.Code, Message: outcome.Message, Alert: outcome.Alert}
}
