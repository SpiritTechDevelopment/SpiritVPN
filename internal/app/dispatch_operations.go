package app

import (
	"context"
	"fmt"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	"github.com/RomanRyabinkin/SpiritVPN/internal/nodeagent"
)

// maxReapedPerStep — сколько протухших lease собирается за один шаг.
//
// Потолок, а не цель: обычно строк ноль. Он нужен на случай перезапуска реплики,
// после которого разом протухают все её lease — собирать их одним неограниченным
// UPDATE значило бы держать длинную транзакцию вопреки §11.1.
const maxReapedPerStep = 100

// Коды исходов, которые производит сам диспетчер, а не агент и не транспорт.
//
// Стоят здесь, а не рядом с набором nodeagent, по принципу «код принадлежит тому,
// кто его порождает»: этих двух отказов агент не видел вовсе. Как и там,
// переименование любого из них — изменение контракта наблюдаемости §15.
const (
	// CodeSuperseded — desired state сменился, пока операция ждала в очереди.
	CodeSuperseded = "SUPERSEDED"
	// CodeCredentialUnreadable — client_uuid не расшифровывается.
	CodeCredentialUnreadable = "CREDENTIAL_UNREADABLE"
)

// DispatchOperations — диспетчер agent-операций (§9).
//
// Единственное место, откуда backend ходит в сеть. Шаг состоит из двух транзакций
// с RPC между ними: §11.1 требует, чтобы ни одна транзакция не оставалась открытой
// во время обращения к node-agent.
//
//	tx1  lease + payload из АКТУАЛЬНЫХ строк access и ноды
//	—    RPC агенту
//	tx2  результат с повторной проверкой desired_version
//
// Payload не хранится в outbox и собирается перед каждой попыткой заново: за время
// ожидания в очереди egress_key access мог смениться (§6, §9).
type DispatchOperations struct {
	Repo   DispatchRepository
	Agent  AgentDispatcher
	Sealer CredentialSealer
	Jitter Jitter

	// Owner попадает в lease_owner: по нему видно, чей lease протух (§15).
	Owner string
	// LeaseTTL должен с запасом перекрывать deadline вызова (§9): lease, истёкший
	// раньше ответа агента, дал бы вторую параллельную операцию на ту же ноду.
	LeaseTTL time.Duration
}

// NewDispatchOperations собирает диспетчер из портов.
func NewDispatchOperations(
	repo DispatchRepository,
	agent AgentDispatcher,
	sealer CredentialSealer,
	jitter Jitter,
	owner string,
	leaseTTL time.Duration,
) *DispatchOperations {
	return &DispatchOperations{
		Repo:     repo,
		Agent:    agent,
		Sealer:   sealer,
		Jitter:   jitter,
		Owner:    owner,
		LeaseTTL: leaseTTL,
	}
}

// ProcessNext доставляет одну операцию.
//
// progressed=false означает, что отправлять нечего: вызывающему циклу стоит
// подождать. Работа может найтись и у сборщика протухших lease — он тоже прогресс,
// потому что вернул в очередь операцию, которую следующий шаг заберёт.
func (uc *DispatchOperations) ProcessNext(ctx context.Context) (progressed bool, err error) {
	reaped, err := uc.Repo.ReapExpiredLeases(ctx, maxReapedPerStep)
	if err != nil {
		return false, fmt.Errorf("сбор протухших lease: %w", err)
	}

	operation, err := uc.Repo.LeaseNext(ctx, uc.Owner, uc.LeaseTTL)
	if err != nil {
		return false, fmt.Errorf("взятие операции: %w", err)
	}
	if operation == nil {
		return reaped > 0, nil
	}

	outcome, sent := uc.deliver(ctx, *operation)
	if !sent {
		// Контекст отменён до или во время вызова. Записывать нечего: операция
		// остаётся IN_FLIGHT, и её подберёт сборщик по истечении lease (решение 51).
		// Попытка записать результат отменённой транзакцией дала бы вторую ошибку
		// поверх первой и ничего не сохранила бы.
		return false, nil
	}

	if err := uc.record(ctx, *operation, outcome); err != nil {
		return false, err
	}
	return true, nil
}

// deliver собирает payload и вызывает агента. sent=false означает, что вызов не
// состоялся из-за отмены контекста.
func (uc *DispatchOperations) deliver(
	ctx context.Context,
	operation LeasedOperation,
) (outcome nodeagent.Outcome, sent bool) {
	if ctx.Err() != nil {
		return nodeagent.Outcome{}, false
	}

	// Проверка перед RPC (§11.1): desired state мог смениться, пока операция ждала
	// в очереди. Звонить агенту незачем — результат всё равно не был бы применён к
	// строке access, а на ноду уехало бы уже неверное состояние.
	//
	// Терминальный статус этой операции назначать здесь не нужно: в tx2
	// SetAccessApplyState по той же несовпавшей версии вернёт fresh=false, и
	// RETRYABLE станет SUPERSEDED тем же механизмом, что и при смене версии уже
	// после RPC (решение 47). Двух путей к одному состоянию не заводим.
	if operation.AccessDesiredVersion > operation.DesiredVersion {
		return nodeagent.Outcome{
			Result:  domain.AttemptRetryable,
			Code:    CodeSuperseded,
			Message: "desired_version access ушла вперёд до отправки",
		}, true
	}

	if operation.DesiredState == domain.DesiredStateAbsent {
		return uc.Agent.EnsureUserAbsent(
			ctx, operation.Endpoint, operation.OperationID.String(), operation.AccountingID), true
	}

	// Открытый client_uuid живёт только внутри этого вызова и в план/лог не
	// попадает (§7, §9).
	clientUUID, err := uc.Sealer.Open(operation.Credential)
	if err != nil {
		// Ключ шифрования не расходится сам собой, и повтор его не починит. Но
		// permanent здесь означал бы, что access не восстановить и сменой desired
		// state: расшифровка провалится и в следующем поколении тоже. Ретраим с
		// alert — по той же логике, что и непригодный agent_config (решение 50).
		return nodeagent.Outcome{
			Result:  domain.AttemptRetryable,
			Code:    CodeCredentialUnreadable,
			Message: err.Error(),
			Alert:   true,
		}, true
	}

	return uc.Agent.EnsureUserPresent(ctx, operation.Endpoint, operation.OperationID.String(), nodeagent.User{
		AccountingID: operation.AccountingID,
		ClientUUID:   clientUUID,
		Flow:         operation.Flow,
		EgressKey:    operation.EgressKey,
	}), true
}

// record записывает исход отдельной транзакцией с повторной проверкой
// desired_version (§11.1).
func (uc *DispatchOperations) record(
	ctx context.Context,
	operation LeasedOperation,
	outcome nodeagent.Outcome,
) error {
	return uc.Repo.WithinResultTx(ctx, func(tx ResultTx) error {
		now, err := tx.Now(ctx)
		if err != nil {
			return fmt.Errorf("время транзакции: %w", err)
		}

		plan := domain.PlanOperationResult(
			outcome.Result, operation.AttemptCount, now, uc.Jitter.Unit())

		fresh, err := tx.SetAccessApplyState(
			ctx, operation.AccessID, operation.DesiredVersion, plan.ApplyState)
		if err != nil {
			return fmt.Errorf("запись apply_state: %w", err)
		}
		if !fresh {
			// desired_version ушла вперёд между RPC и записью: строка access не
			// тронута, а повторять операцию устаревшей версии нельзя (решение 47).
			plan = plan.Superseded()
		}

		if err := tx.CompleteOperation(ctx, OperationResult{
			OperationID:   operation.OperationID,
			Status:        plan.Status,
			NextAttemptAt: plan.NextAttemptAt,
			Completed:     plan.Completed,
			ErrorCode:     outcome.Code,
			ErrorMessage:  outcome.Message,
		}); err != nil {
			return fmt.Errorf("запись результата операции: %w", err)
		}
		return nil
	})
}
