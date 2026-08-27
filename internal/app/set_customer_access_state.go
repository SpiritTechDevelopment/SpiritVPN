package app

import (
	"context"
	"fmt"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
)

type SetCustomerAccessStateCommand struct {
	Command   domain.AdministrativeCommand
	Actor     string
	RequestID string
}

type SetCustomerAccessState struct {
	Repo LifecycleRepository
	IDs  IDs
}

func NewSetCustomerAccessState(repo LifecycleRepository, ids IDs) *SetCustomerAccessState {
	return &SetCustomerAccessState{Repo: repo, IDs: ids}
}

func (uc *SetCustomerAccessState) Execute(ctx context.Context, request SetCustomerAccessStateCommand) error {
	cmd := request.Command
	if err := domain.ValidateAdministrativeCommand(cmd); err != nil {
		return err
	}
	fingerprint := commandFingerprint("SetCustomerAccessState", cmd.CustomerID, cmd.CommandNumber,
		uint64(lifecycleOrdinal(cmd.Target)))

	return uc.Repo.WithinLifecycleTx(ctx, func(tx LifecycleTx) error {
		ent, err := tx.LockEntitlement(ctx, cmd.CustomerID)
		if err != nil {
			return fmt.Errorf("блокировка entitlement: %w", err)
		}
		if ent == nil || domain.EffectiveLifecycle(ent) == domain.CustomerLifecycleDeleted {
			return domain.ErrCustomerNotFound
		}
		order, err := domain.ClassifyCommand(cmd.CommandNumber, fingerprint, ent)
		if err != nil {
			return err
		}
		if order != domain.CommandNew {
			return nil
		}
		if domain.EffectiveLifecycle(ent) == domain.CustomerLifecycleDeleting {
			return domain.ErrCustomerDeleting
		}

		accesses, err := tx.LoadAccesses(ctx, cmd.CustomerID)
		if err != nil {
			return fmt.Errorf("чтение access: %w", err)
		}
		var changes []domain.DesiredChange
		var liveNodes []domain.NodeID
		if cmd.Target == domain.CustomerLifecycleBlocked && domain.EffectiveLifecycle(ent) != cmd.Target {
			changes = domain.PlanForceAbsent(accesses)
			liveNodes, err = tx.LoadLiveNodes(ctx)
			if err != nil {
				return fmt.Errorf("чтение актуальных нод: %w", err)
			}
		}
		if cmd.Target == domain.CustomerLifecycleActive && domain.EffectiveLifecycle(ent) != cmd.Target {
			now, nowErr := tx.Now(ctx)
			if nowErr != nil {
				return fmt.Errorf("время транзакции: %w", nowErr)
			}
			period, periodErr := tx.LockOpenQuotaPeriod(ctx, cmd.CustomerID)
			if periodErr != nil {
				return fmt.Errorf("блокировка периода квоты: %w", periodErr)
			}
			if period == nil {
				return domain.ErrOpenPeriodMissing
			}
			usage, usageErr := tx.LockNodeQuotaUsage(ctx, period.ID)
			if usageErr != nil {
				return fmt.Errorf("блокировка расхода: %w", usageErr)
			}
			exhausted := make(map[domain.NodeID]bool, len(usage))
			for _, item := range usage {
				exhausted[item.NodeID] = domain.IsQuotaExhausted(item.TotalBytes, period.UsageQuotaBytes)
			}
			topology, topologyErr := tx.LoadTopology(ctx, ent.FleetID)
			if topologyErr != nil {
				return fmt.Errorf("чтение топологии: %w", topologyErr)
			}
			inSync := domain.PlanAccessSet(topology, accesses).InSync
			changes = domain.PlanDesiredChangesForLifecycle(inSync, domain.CustomerLifecycleActive, now, ent.ExpiresAt, exhausted)
		}

		plan, err := uc.materialize(cmd.CustomerID, cmd.Target, cmd.CommandNumber, fingerprint,
			ent.DesiredVersion, changes, liveNodes, cmd.Target == domain.CustomerLifecycleBlocked, nil)
		if err != nil {
			return err
		}
		if err := tx.WriteLifecycle(ctx, plan); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, AuditEvent{
			ActorType: auditActorTypeAccessAdmin, ActorID: request.Actor,
			Action: auditActionCustomerStateSet, TargetType: auditTargetTypeCustomer,
			TargetID: cmd.CustomerID, RequestID: request.RequestID, Outcome: auditOutcomeAccepted,
			Metadata: map[string]any{"state": cmd.Target, "command_number": cmd.CommandNumber, "changed_access": len(changes)},
		})
	})
}

func (uc *SetCustomerAccessState) materialize(
	customerID string, target domain.CustomerLifecycle, number uint64, fingerprint []byte,
	desiredVersion int64, changes []domain.DesiredChange, liveNodes []domain.NodeID, restrictToLiveNodes bool,
	deleteNotBefore *time.Time,
) (MaterializedLifecyclePlan, error) {
	plan := MaterializedLifecyclePlan{
		CustomerID: customerID, Target: target, CommandNumber: number,
		CommandFingerprint: fingerprint, DesiredVersion: desiredVersion,
		DeleteNotBefore: deleteNotBefore, DesiredChanges: changes,
	}
	if len(changes) > 0 {
		plan.DesiredVersion++
	}
	live := make(map[domain.NodeID]struct{}, len(liveNodes))
	for _, node := range liveNodes {
		live[node] = struct{}{}
	}
	issued := make([]domain.DesiredChange, 0, len(changes))
	for _, change := range changes {
		if restrictToLiveNodes {
			if _, ok := live[change.EntryNodeID]; !ok {
				plan.AppliedWithoutOperation = append(plan.AppliedWithoutOperation, change.AccessID)
				continue
			}
		}
		id, err := uc.IDs.NewOperationID()
		if err != nil {
			return MaterializedLifecyclePlan{}, fmt.Errorf("operation_id: %w", err)
		}
		plan.Operations = append(plan.Operations, AgentOperation{
			OperationID: id, NodeID: change.EntryNodeID, AccessID: change.AccessID,
			DesiredState: change.DesiredState, DesiredVersion: change.DesiredVersion,
		})
		issued = append(issued, change)
	}
	plan.TouchedNodes = domain.TouchedNodesForChanges(issued)
	return plan, nil
}

func lifecycleOrdinal(state domain.CustomerLifecycle) int {
	if state == domain.CustomerLifecycleActive {
		return 1
	}
	if state == domain.CustomerLifecycleBlocked {
		return 2
	}
	return 0
}
