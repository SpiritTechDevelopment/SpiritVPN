package app

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
)

// ApplyCustomerCommand — доменная команда вместе с тем, что нужно аудиту.
//
// Actor и RequestID не входят в domain.ApplyCommand намеренно: домен от них не
// зависит ни одним правилом, а попав туда, они стали бы частью значения, по
// которому сравниваются повторы команд. Та же форма, что и у
// ApplyManifestCommand.
type ApplyCustomerCommand struct {
	Command domain.ApplyCommand
	// Actor — идентичность вызывающего из mTLS. Пустая допустима: тесты и
	// будущие внутренние вызовы актора не имеют, а колонка nullable.
	Actor string
	// RequestID связывает запись журнала с логами того же запроса.
	RequestID string
}

// ApplyCustomerAccess — use case команды ApplyCustomerAccess.
//
// Одна короткая транзакция без единого сетевого вызова агентам: операции только
// кладутся в outbox, а отправляет их dispatcher уже после commit. Успешный
// ответ означает, что desired state и durable operations зафиксированы в
// PostgreSQL, но не то, что агенты их уже применили.
type ApplyCustomerAccess struct {
	Repo   ApplyRepository
	IDs    IDs
	Sealer CredentialSealer
}

// NewApplyCustomerAccess собирает use case из портов.
func NewApplyCustomerAccess(repo ApplyRepository, ids IDs, sealer CredentialSealer) *ApplyCustomerAccess {
	return &ApplyCustomerAccess{Repo: repo, IDs: ids, Sealer: sealer}
}

// Execute применяет одну команду.
//
// Порядок шагов нормативен и держится здесь, а не в SQL:
//
//  1. валидация формы — до любого обращения к БД, чтобы невалидный запрос не
//     блокировал корневую строку и не двигал last_command_number;
//  2. SELECT now() — первым оператором транзакции;
//  3. lock корневой строки; её отсутствие означает нового customer;
//  4. отсечение устаревшей команды — идемпотентный OK без единого side effect;
//  5. проверка fleet — только после шага 4, иначе повтор старой команды для
//     исчезнувшего из manifest fleet вернул бы NOT_FOUND вместо OK;
//  6. планирование и запись; last_command_number двигается при успешном commit,
//     в том числе на валидном no-op;
//  7. запись аудита — в той же транзакции, поэтому откат уносит и её.
func (uc *ApplyCustomerAccess) Execute(ctx context.Context, request ApplyCustomerCommand) error {
	cmd := request.Command

	// Шаг 1.
	if err := domain.ValidateApplyCommand(cmd); err != nil {
		return err
	}

	return uc.Repo.WithinTx(ctx, func(tx ApplyTx) error {
		// Шаг 2.
		now, err := tx.Now(ctx)
		if err != nil {
			return fmt.Errorf("время транзакции: %w", err)
		}

		// Шаг 3.
		entitlement, err := tx.LockEntitlement(ctx, cmd.CustomerID)
		if err != nil {
			return fmt.Errorf("блокировка entitlement: %w", err)
		}

		// Шаг 4. Возврат nil коммитит пустую транзакцию: команда уже применена или
		// переупорядочена, и повторять её side effects нельзя.
		if domain.IsStaleCommand(cmd, entitlement) {
			return nil
		}

		// Шаг 5.
		current, err := tx.FleetIsCurrent(ctx, cmd.FleetID)
		if err != nil {
			return fmt.Errorf("поиск fleet: %w", err)
		}
		if !current {
			return domain.ErrFleetNotFound
		}

		// Шаг 6. Порядок чтений совпадает с порядком блокировок: период и
		// расход берутся FOR UPDATE до того, как транзакция дойдёт до нод, access и
		// операций в WritePlan.
		input := domain.ApplyInput{
			Now:         now,
			Command:     cmd,
			Entitlement: entitlement,
		}

		if entitlement != nil {
			period, periodErr := tx.LockOpenQuotaPeriod(ctx, cmd.CustomerID)
			if periodErr != nil {
				return fmt.Errorf("блокировка периода квоты: %w", periodErr)
			}
			input.OpenPeriod = period

			if period != nil {
				usage, usageErr := tx.LockNodeQuotaUsage(ctx, period.ID)
				if usageErr != nil {
					return fmt.Errorf("блокировка расхода по нодам: %w", usageErr)
				}
				input.NodeUsage = usage
			}
		}

		topology, err := tx.LoadTopology(ctx, cmd.FleetID)
		if err != nil {
			return fmt.Errorf("чтение топологии: %w", err)
		}
		input.Topology = topology

		accesses, err := tx.LoadAccesses(ctx, cmd.CustomerID)
		if err != nil {
			return fmt.Errorf("чтение access: %w", err)
		}
		input.Accesses = accesses

		plan, err := domain.PlanApply(input)
		if err != nil {
			return err
		}

		materialized, err := uc.materialize(cmd, plan, input.OpenPeriod)
		if err != nil {
			return err
		}

		if err := tx.WritePlan(ctx, materialized); err != nil {
			return err
		}

		// Шаг 7. Аудируется всё принятое, включая пустой план: команда принята и
		// last_command_number сдвинулся, а «backend принял команду N» — ровно тот
		// факт, который потом сверяют с product-сервисом. Устаревшая команда сюда
		// не доходит (шаг 4), и правильно: side effects у неё нет, а повтор
		// доставки писал бы дубликаты одного и того же no-op.
		event, err := customerAudit(request, plan)
		if err != nil {
			return err
		}
		return tx.AppendAudit(ctx, event)
	})
}

// customerActions — действие журнала по решению домена.
//
// Словарь, а не switch с default: промах означает, что домен завёл четвёртое
// решение, а запись с action "UNKNOWN" молча увезла бы это расхождение в журнал,
// по которому потом разбирают инциденты.
var customerActions = map[domain.ApplyDecision]string{
	domain.ApplyDecisionCreate:      auditActionCustomerCreated,
	domain.ApplyDecisionRenewal:     auditActionCustomerRenewed,
	domain.ApplyDecisionQuotaChange: auditActionCustomerQuotaChanged,
}

// customerAudit собирает запись о принятой команде customer.
//
// В метаданных только счётчики и лимиты. Ни accounting_id, ни client_uuid:
// запрещает секреты в журнале, а customer_id, наоборот, разрешён именно здесь и
// уезжает в target_id.
func customerAudit(request ApplyCustomerCommand, plan domain.ApplyPlan) (AuditEvent, error) {
	action, ok := customerActions[plan.Decision]
	if !ok {
		return AuditEvent{}, fmt.Errorf("app: нет действия аудита для решения %s", plan.Decision)
	}

	return AuditEvent{
		ActorType:  auditActorTypeAccessWriter,
		ActorID:    request.Actor,
		Action:     action,
		TargetType: auditTargetTypeCustomer,
		TargetID:   request.Command.CustomerID,
		RequestID:  request.RequestID,
		Outcome:    auditOutcomeAccepted,
		Metadata: map[string]any{
			"command_number":   request.Command.CommandNumber,
			"expires_at":       plan.ExpiresAt.UTC().Format(time.RFC3339),
			"quota_bytes":      plan.QuotaBytes,
			"new_quota_period": plan.OpenNewPeriod,
			"created_access":   len(plan.CreateAccesses),
			"changed_access":   len(plan.DesiredChanges),
			"affected_nodes":   len(plan.TouchedNodes),
		},
	}, nil
}

// materialize дополняет доменный план идентификаторами и credentials.
//
// Sealing выполняется внутри транзакции. Криптографию положено выносить
// наружу, но число создаваемых access известно только после чтения топологии и
// текущего набора под lock, поэтому без гонки вынести Seal нельзя. Фактическая
// цена — AES-GCM над 16 байтами не более (nodes + relations) раз на команду, то
// есть при ≤10 нод и ≤90 связей на fleet это единицы микросекунд против сетевого
// RTT до PostgreSQL в той же транзакции. Смысл требования — никаких сетевых
// вызовов и тяжёлой работы под lock — при этом не нарушен.
func (uc *ApplyCustomerAccess) materialize(
	cmd domain.ApplyCommand,
	plan domain.ApplyPlan,
	openPeriod *domain.QuotaPeriod,
) (MaterializedPlan, error) {
	materialized := MaterializedPlan{
		CustomerID:    cmd.CustomerID,
		FleetID:       cmd.FleetID,
		CommandNumber: cmd.CommandNumber,
		Plan:          plan,
	}

	periodID, err := uc.periodID(plan, openPeriod)
	if err != nil {
		return MaterializedPlan{}, err
	}
	materialized.PeriodID = periodID

	for _, spec := range plan.CreateAccesses {
		access, accessErr := uc.newAccess(spec)
		if accessErr != nil {
			return MaterializedPlan{}, accessErr
		}
		materialized.NewAccesses = append(materialized.NewAccesses, access)

		// Операция создаётся только для PRESENT: юзера на ноде ещё не было, и
		// отсутствие уже является состоянием Xray по умолчанию.
		if spec.DesiredState != domain.DesiredStatePresent {
			continue
		}
		operation, opErr := uc.newOperation(spec.EntryNodeID, access.AccessID, spec.DesiredState, spec.DesiredVersion)
		if opErr != nil {
			return MaterializedPlan{}, opErr
		}
		materialized.Operations = append(materialized.Operations, operation)
	}

	// Смена desired state существующего access всегда порождает операцию: и
	// EnsureUserPresent, и EnsureUserAbsent.
	for _, change := range plan.DesiredChanges {
		operation, opErr := uc.newOperation(change.EntryNodeID, change.AccessID, change.DesiredState, change.DesiredVersion)
		if opErr != nil {
			return MaterializedPlan{}, opErr
		}
		materialized.Operations = append(materialized.Operations, operation)
	}

	return materialized, nil
}

// periodID выбирает период, в который пишутся изменения квоты: новый при renewal и
// создании, уже открытый при изменении лимита.
func (uc *ApplyCustomerAccess) periodID(plan domain.ApplyPlan, openPeriod *domain.QuotaPeriod) (uuid.UUID, error) {
	if plan.OpenNewPeriod {
		id, err := uc.IDs.NewQuotaPeriodID()
		if err != nil {
			return uuid.Nil, err
		}
		return id, nil
	}

	if openPeriod == nil {
		// Недостижимо: PlanApply возвращает ErrOpenPeriodMissing раньше. Проверка
		// оставлена, чтобы будущее изменение домена не привело к записи в
		// uuid.Nil-период.
		return uuid.Nil, domain.ErrOpenPeriodMissing
	}
	return openPeriod.ID, nil
}

// newAccess выдаёт создаваемому access идентичность и credential. Каждое новое
// поколение цели получает новые client_uuid и accounting_id; существующие access их
// сохраняют.
func (uc *ApplyCustomerAccess) newAccess(spec domain.NewAccessSpec) (NewAccess, error) {
	accessID, err := uc.IDs.NewAccessID()
	if err != nil {
		return NewAccess{}, err
	}
	accountingID, err := uc.IDs.NewAccountingID()
	if err != nil {
		return NewAccess{}, err
	}
	clientUUID, err := uc.IDs.NewClientUUID()
	if err != nil {
		return NewAccess{}, err
	}
	credential, err := uc.Sealer.Seal(clientUUID)
	if err != nil {
		return NewAccess{}, err
	}

	return NewAccess{
		Spec:         spec,
		AccessID:     accessID,
		AccountingID: accountingID,
		Credential:   credential,
	}, nil
}

func (uc *ApplyCustomerAccess) newOperation(
	nodeID domain.NodeID,
	accessID uuid.UUID,
	desiredState domain.DesiredState,
	desiredVersion int64,
) (AgentOperation, error) {
	operationID, err := uc.IDs.NewOperationID()
	if err != nil {
		return AgentOperation{}, err
	}

	return AgentOperation{
		OperationID:    operationID,
		NodeID:         nodeID,
		AccessID:       accessID,
		DesiredState:   desiredState,
		DesiredVersion: desiredVersion,
	}, nil
}
