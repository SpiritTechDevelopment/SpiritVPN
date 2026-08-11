package app

import (
	"context"
	"fmt"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
)

// ExpireCustomers — воркер истечения срока (§13).
//
// Единственный, кто снимает доступ по времени: команды ручного enable/disable в v1
// нет, а `GetCustomerAccessLinks` лишь перестаёт отдавать URI (§4, §5). Без этого
// воркера Xray-юзер истёкшего customer остался бы на ноде навсегда.
//
// Сетевых вызовов здесь нет: операции кладутся в outbox, доставляет их
// DispatchOperations (§9).
type ExpireCustomers struct {
	Repo ExpiryRepository
	IDs  IDs
}

// NewExpireCustomers собирает воркер из портов.
func NewExpireCustomers(repo ExpiryRepository, ids IDs) *ExpireCustomers {
	return &ExpireCustomers{Repo: repo, IDs: ids}
}

// ProcessNext гасит доступ ОДНОГО истёкшего customer.
//
// Шаг маленький по тем же соображениям, что и у материализации: §11.1 «сначала row
// lock корневой строки» выполняется тривиально, и не нужно упорядочивать блокировки
// нескольких customer внутри одной транзакции.
//
// progressed=false означает, что гасить некого: вызывающему циклу стоит подождать.
func (uc *ExpireCustomers) ProcessNext(ctx context.Context) (progressed bool, err error) {
	err = uc.Repo.WithinExpiryTx(ctx, func(tx ExpiryTx) error {
		now, nowErr := tx.Now(ctx)
		if nowErr != nil {
			return fmt.Errorf("время транзакции: %w", nowErr)
		}

		due, dueErr := tx.LockNextDueCustomer(ctx)
		if dueErr != nil {
			return fmt.Errorf("выбор истёкшего customer: %w", dueErr)
		}
		if due == nil {
			return nil
		}

		accesses, accessErr := tx.LoadAccesses(ctx, due.CustomerID)
		if accessErr != nil {
			return fmt.Errorf("чтение access: %w", accessErr)
		}

		// expires_at берётся из строки, прочитанной ПОД locком, и план строится от
		// него: §11.1 требует этой перепроверки, чтобы expiry не снял доступ после
		// уже закоммиченного renewal (решение 57).
		plan := domain.PlanExpiry(now, due.Entitlement, accesses)
		if plan.IsNoOp() {
			// Продлённый customer. Шаг засчитывается прогрессом: работа была
			// проделана, а следующая выборка его уже не найдёт.
			progressed = true
			return nil
		}

		materialized, matErr := uc.materializeExpiryPlan(due.CustomerID, plan)
		if matErr != nil {
			return matErr
		}
		if err := tx.WriteExpiry(ctx, materialized); err != nil {
			return err
		}

		progressed = true
		return tx.AppendAudit(ctx, expiryAudit(due, plan))
	})
	if err != nil {
		return false, err
	}
	return progressed, nil
}

// materializeExpiryPlan выдаёт каждому погашенному access операцию удаления.
func (uc *ExpireCustomers) materializeExpiryPlan(
	customerID string,
	plan domain.ExpiryPlan,
) (MaterializedExpiryPlan, error) {
	materialized := MaterializedExpiryPlan{CustomerID: customerID, Plan: plan}

	for _, change := range plan.DesiredChanges {
		operationID, err := uc.IDs.NewOperationID()
		if err != nil {
			return MaterializedExpiryPlan{}, err
		}

		materialized.Operations = append(materialized.Operations, AgentOperation{
			OperationID:    operationID,
			NodeID:         change.EntryNodeID,
			AccessID:       change.AccessID,
			DesiredState:   change.DesiredState,
			DesiredVersion: change.DesiredVersion,
		})
	}

	return materialized, nil
}

// expiryAudit — запись журнала о снятии доступа (§15).
//
// Причина TIME_EXPIRED выводится из expires_at и отдельной строкой нигде не
// хранится (§13), поэтому журнал — единственное место, где момент срабатывания
// остаётся видимым постфактум.
//
// Метаданные санитизированы: ни accounting ID, ни client_uuid, только счётчики.
// customer_id допустим — §15 разрешает его именно в audit records.
func expiryAudit(due *ExpiredCustomer, plan domain.ExpiryPlan) AuditEvent {
	return AuditEvent{
		ActorType:  auditActorTypeSystem,
		Action:     auditActionCustomerExpired,
		TargetType: auditTargetTypeCustomer,
		TargetID:   due.CustomerID,
		Outcome:    auditOutcomeAccepted,
		Metadata: map[string]any{
			"expires_at":     due.Entitlement.ExpiresAt.UTC().Format(time.RFC3339),
			"revoked_access": len(plan.DesiredChanges),
			"affected_nodes": len(plan.TouchedNodes),
		},
	}
}
