package app

import (
	"context"
	"fmt"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
)

type DeleteCustomerAccessCommand struct {
	Command   domain.DeleteCommand
	Actor     string
	RequestID string
}

type DeleteCustomerAccess struct {
	Repo  LifecycleRepository
	IDs   IDs
	Grace time.Duration
}

func NewDeleteCustomerAccess(repo LifecycleRepository, ids IDs, grace time.Duration) *DeleteCustomerAccess {
	return &DeleteCustomerAccess{Repo: repo, IDs: ids, Grace: grace}
}

func (uc *DeleteCustomerAccess) Execute(
	ctx context.Context,
	request DeleteCustomerAccessCommand,
) (domain.CustomerDeletionState, error) {
	cmd := request.Command
	if err := domain.ValidateDeleteCommand(cmd); err != nil {
		return "", err
	}
	fingerprint := commandFingerprint("DeleteCustomerAccess", cmd.CustomerID, cmd.CommandNumber)
	state := domain.CustomerDeletionPending

	err := uc.Repo.WithinLifecycleTx(ctx, func(tx LifecycleTx) error {
		ent, err := tx.LockEntitlement(ctx, cmd.CustomerID)
		if err != nil {
			return fmt.Errorf("блокировка entitlement: %w", err)
		}
		if ent == nil {
			if err := tx.InsertDeletedTombstone(ctx, cmd.CustomerID, cmd.CommandNumber, fingerprint); err != nil {
				return err
			}
			state = domain.CustomerDeletionCompleted
			return tx.AppendAudit(ctx, deletionAudit(request, state, 0))
		}

		order, err := domain.ClassifyCommand(cmd.CommandNumber, fingerprint, ent)
		if err != nil {
			return err
		}
		current := domain.EffectiveLifecycle(ent)
		if order != domain.CommandNew {
			if current != domain.CustomerLifecycleDeleting {
				state = domain.CustomerDeletionCompleted
			}
			return nil
		}
		if current == domain.CustomerLifecycleDeleted {
			// Новый Delete после уже завершённого удаления только двигает ordering
			// token; operational data создавать заново незачем.
			plan := MaterializedLifecyclePlan{
				CustomerID: cmd.CustomerID, Target: domain.CustomerLifecycleDeleted,
				CommandNumber: cmd.CommandNumber, CommandFingerprint: fingerprint,
				DesiredVersion: ent.DesiredVersion,
			}
			if err := tx.WriteLifecycle(ctx, plan); err != nil {
				return err
			}
			state = domain.CustomerDeletionCompleted
			return tx.AppendAudit(ctx, deletionAudit(request, state, 0))
		}

		accesses, err := tx.LoadAccesses(ctx, cmd.CustomerID)
		if err != nil {
			return fmt.Errorf("чтение access: %w", err)
		}
		changes := domain.PlanForceAbsent(accesses)
		liveNodes, err := tx.LoadLiveNodes(ctx)
		if err != nil {
			return fmt.Errorf("чтение актуальных нод: %w", err)
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return fmt.Errorf("время транзакции: %w", err)
		}
		notBefore := now.Add(uc.Grace)
		materializer := SetCustomerAccessState{IDs: uc.IDs}
		plan, err := materializer.materialize(cmd.CustomerID, domain.CustomerLifecycleDeleting,
			cmd.CommandNumber, fingerprint, ent.DesiredVersion, changes, liveNodes, true, &notBefore)
		if err != nil {
			return err
		}
		if err := tx.WriteLifecycle(ctx, plan); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, deletionAudit(request, state, len(changes)))
	})
	return state, err
}

func deletionAudit(request DeleteCustomerAccessCommand, state domain.CustomerDeletionState, changed int) AuditEvent {
	return AuditEvent{
		ActorType: auditActorTypeAccessAdmin, ActorID: request.Actor,
		Action: auditActionCustomerDelete, TargetType: auditTargetTypeCustomer,
		TargetID: request.Command.CustomerID, RequestID: request.RequestID, Outcome: auditOutcomeAccepted,
		Metadata: map[string]any{"command_number": request.Command.CommandNumber, "state": state, "changed_access": changed},
	}
}

// FinalizeCustomerDeletions — restart-safe cleanup worker.
type FinalizeCustomerDeletions struct{ Repo DeletionRepository }

func NewFinalizeCustomerDeletions(repo DeletionRepository) *FinalizeCustomerDeletions {
	return &FinalizeCustomerDeletions{Repo: repo}
}

func (uc *FinalizeCustomerDeletions) ProcessNext(ctx context.Context) (bool, error) {
	return uc.Repo.FinalizeNextDeletion(ctx)
}
