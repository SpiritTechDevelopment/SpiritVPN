package app

import (
	"context"
	"fmt"

	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
)

// ApplyManifestCommand — принятый к обработке снапшот вместе с тем, что нужно
// аудиту.
//
// Actor и RequestID приходят с транспорта, а не добываются из контекста внутри:
// use case не должен знать ни про mTLS, ни про метадату gRPC, а §15 требует
// в audit records и того, и другого.
type ApplyManifestCommand struct {
	Snapshot         domain.ManifestSnapshot
	AllowDestructive bool
	Actor            string
	RequestID        string
}

// ApplyManifestResult — исход приёма (§6).
type ApplyManifestResult struct {
	// Revision, ставшая текущей; на идемпотентном повторе — эхо запроса.
	Revision int64
	// Idempotent — та же revision с тем же digest, ничего не записано.
	Idempotent bool
}

// ApplyFleetManifest — use case приёма infrastructure manifest (§6).
//
// Весь снапшот применяется одной транзакцией либо не применяется вовсе. Fan-out
// customer access внутрь этой транзакции не входит: он идёт durable-джобой уже
// после commit (§6, §13), потому что 50 000 customer в одной транзакции с RPC
// нарушили бы требование §11.1 о коротких транзакциях.
type ApplyFleetManifest struct {
	Repo ManifestRepository
}

// NewApplyFleetManifest собирает use case из порта.
func NewApplyFleetManifest(repo ManifestRepository) *ApplyFleetManifest {
	return &ApplyFleetManifest{Repo: repo}
}

// Execute валидирует и применяет снапшот (§6).
//
// Порядок шагов нормативен:
//
//  1. валидация формы — до транзакции: заведомо невалидный снапшот не должен ни
//     брать advisory lock, ни читать проекцию целиком;
//  2. чтение принятого состояния и планирование — под lock приёма;
//  3. запись; на идемпотентном повторе не пишется ничего, включая джобу
//     материализации (решение 21);
//  4. аудит destructive-приёма в той же транзакции (§15): запись, пережившая
//     откат проекции, соврала бы о том, что удаление состоялось.
func (uc *ApplyFleetManifest) Execute(
	ctx context.Context,
	cmd ApplyManifestCommand,
) (ApplyManifestResult, error) {
	// Шаг 1.
	if err := domain.ValidateManifest(cmd.Snapshot); err != nil {
		return ApplyManifestResult{}, err
	}

	var result ApplyManifestResult

	err := uc.Repo.WithinManifestTx(ctx, func(tx ManifestTx) error {
		// Шаг 2.
		projection, err := tx.LoadProjection(ctx)
		if err != nil {
			return fmt.Errorf("чтение проекции манифеста: %w", err)
		}

		plan, err := domain.PlanManifest(domain.ManifestInput{
			Snapshot:         cmd.Snapshot,
			AllowDestructive: cmd.AllowDestructive,
			Projection:       projection,
		})
		if err != nil {
			return err
		}

		result = ApplyManifestResult{Revision: plan.Revision, Idempotent: plan.Idempotent}
		if plan.Idempotent {
			return nil
		}

		// Шаг 3.
		if err := tx.WritePlan(ctx, plan); err != nil {
			return fmt.Errorf("запись проекции манифеста: %w", err)
		}

		// Шаг 4.
		if !plan.Destructive {
			return nil
		}
		if err := tx.AppendAudit(ctx, destructiveAudit(cmd, plan)); err != nil {
			return fmt.Errorf("запись аудита: %w", err)
		}
		return nil
	})
	if err != nil {
		return ApplyManifestResult{}, err
	}

	return result, nil
}

// destructiveAudit собирает запись о принятом destructive-манифесте (§15).
//
// В метаданных только счётчики и идентификаторы топологии: секретов и customer ID
// здесь нет и быть не может — приём манифеста их вообще не видит (§15 допускает
// customer ID лишь в ограниченных audit records, и этот к ним не относится).
func destructiveAudit(cmd ApplyManifestCommand, plan domain.ManifestPlan) AuditEvent {
	return AuditEvent{
		ActorType:  auditActorTypeManifestWriter,
		ActorID:    cmd.Actor,
		Action:     auditActionManifestApplied,
		TargetType: auditTargetTypeManifest,
		TargetID:   fmt.Sprint(plan.Revision),
		RequestID:  cmd.RequestID,
		Outcome:    auditOutcomeAccepted,
		Metadata: map[string]any{
			"destructive":         true,
			"removed_nodes":       plan.RemovedNodes,
			"removed_memberships": plan.RemovedMemberships,
			"removed_bridges":     plan.RemovedBridges,
		},
	}
}
