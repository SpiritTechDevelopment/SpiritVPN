package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	"github.com/RomanRyabinkin/SpiritVPN/internal/postgres/db"
)

// WithinMaterializationTx выполняет один шаг воркера в одной транзакции.
//
// READ COMMITTED с явными row locks, как и командный путь: шаг меняет
// состояние одного customer и подчиняется тому же порядку блокировок.
func (r *Repository) WithinMaterializationTx(ctx context.Context, fn func(app.MaterializationTx) error) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("postgres: начать транзакцию материализации: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(&materializeTx{applyTx: applyTx{queries: db.New(tx)}}); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit материализации: %w", err)
	}
	return nil
}

// materializeTx встраивает applyTx ради чтений состояния customer.
//
// Встраивание, а не копия: LockEntitlement, LockOpenQuotaPeriod,
// LockNodeQuotaUsage, LoadTopology, LoadAccesses и Now у обоих путей буквально
// одни и те же, и вторая реализация со временем разъехалась бы с первой. Свои
// методы ниже — только то, чего у командного пути нет.
type materializeTx struct {
	applyTx
}

// ClaimJob берёт lease самой старой незавершённой джобы. nil означает, что
// работы нет.
func (t *materializeTx) ClaimJob(
	ctx context.Context,
	owner string,
	leaseTTL time.Duration,
) (*app.MaterializationJob, error) {
	// Границы проверяются до сужения: отрицательный или абсурдно большой TTL
	// после приведения к int32 дал бы негодный lease вместо явной ошибки.
	seconds := leaseTTL.Seconds()
	if seconds < 1 || seconds > math.MaxInt32 {
		return nil, fmt.Errorf("postgres: недопустимый lease TTL %s", leaseTTL)
	}

	row, err := t.queries.ClaimMaterializationJob(ctx, db.ClaimMaterializationJobParams{
		Owner:        owner,
		LeaseSeconds: int32(seconds),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return &app.MaterializationJob{Revision: row.Revision, Cursor: row.CursorCustomerID}, nil
}

// NextCustomer возвращает пустую строку, когда обход завершён.
func (t *materializeTx) NextCustomer(ctx context.Context, afterCustomerID string) (string, error) {
	customerID, err := t.queries.NextCustomerAfter(ctx, afterCustomerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return customerID, nil
}

func (t *materializeTx) AdvanceCursor(ctx context.Context, revision int64, customerID string) error {
	return t.queries.AdvanceMaterializationCursor(ctx, db.AdvanceMaterializationCursorParams{
		Revision:   revision,
		CustomerID: customerID,
	})
}

func (t *materializeTx) CompleteJob(ctx context.Context, revision int64) error {
	return t.queries.CompleteMaterializationJob(ctx, revision)
}

// LoadLiveNodes — ноды текущего манифеста глобально, а не только внутри fleet.
// Тот же запрос, что читает проекцию приём манифеста.
func (t *materializeTx) LoadLiveNodes(ctx context.Context) ([]domain.NodeID, error) {
	nodes, err := t.queries.ListCurrentNodeIDs(ctx)
	if err != nil {
		return nil, err
	}
	return nodeIDsFromStrings(nodes), nil
}

// WriteMaterialization записывает план в нормативном порядке блокировок:
//
//  1. customer_entitlements
//  3. node_quota_usage
//  5. vpn_nodes
//  6. vpn_accesses
//  7. agent_operations
//
// Шаг 2 (quota_periods) пропущен: материализация период не создаёт и не
// закрывает — она лишь дописывает в открытый строки расхода новых нод.
func (t *materializeTx) WriteMaterialization(ctx context.Context, plan app.MaterializedManifestPlan) error {
	if err := t.queries.BumpEntitlementDesiredVersion(ctx, db.BumpEntitlementDesiredVersionParams{
		CustomerID:     plan.CustomerID,
		DesiredVersion: plan.Plan.EntitlementDesiredVersion,
	}); err != nil {
		return err
	}

	if len(plan.Plan.NodeQuotaInits) > 0 {
		if err := t.queries.InsertNodeQuotaUsage(ctx, db.InsertNodeQuotaUsageParams{
			QuotaPeriodID: plan.PeriodID,
			NodeIds:       nodeIDStrings(plan.Plan.NodeQuotaInits),
		}); err != nil {
			return err
		}
	}

	if err := t.writeTouchedNodes(ctx, plan.Plan.TouchedNodes); err != nil {
		return err
	}
	if err := t.writeMaterializedAccesses(ctx, plan); err != nil {
		return err
	}
	return t.writeMaterializedOperations(ctx, plan)
}

// writeTouchedNodes блокирует ноды в порядке node_id и ровно один раз
// увеличивает каждой desired_revision.
func (t *materializeTx) writeTouchedNodes(ctx context.Context, nodes []domain.NodeID) error {
	if len(nodes) == 0 {
		return nil
	}

	nodeIDs := nodeIDStrings(nodes)

	locked, err := t.queries.LockNodesForUpdate(ctx, nodeIDs)
	if err != nil {
		return err
	}
	if len(locked) != len(nodeIDs) {
		return fmt.Errorf("postgres: заблокировано %d нод из %d — проекция топологии рассогласована",
			len(locked), len(nodeIDs))
	}

	return t.queries.BumpNodesDesiredRevision(ctx, nodeIDs)
}

// writeMaterializedAccesses создаёт, гасит, смещает и ретайрит access. Все списки
// плана отсортированы по access_id.
func (t *materializeTx) writeMaterializedAccesses(ctx context.Context, plan app.MaterializedManifestPlan) error {
	for _, access := range plan.NewAccesses {
		if err := t.queries.InsertVpnAccess(ctx, db.InsertVpnAccessParams{
			AccessID:            access.AccessID,
			CustomerID:          plan.CustomerID,
			Kind:                string(access.Spec.Kind),
			LogicalTargetKey:    string(access.Spec.LogicalTargetKey),
			Generation:          access.Spec.Generation,
			EntryNodeID:         string(access.Spec.EntryNodeID),
			EgressKey:           access.Spec.EgressKey,
			AccountingID:        access.AccountingID,
			EncryptedClientUuid: access.Credential.Blob,
			EncryptionKeyID:     access.Credential.KeyID,
			DesiredState:        string(access.Spec.DesiredState),
			ApplyState:          applyStateForNewAccess(access.Spec.DesiredState),
			DesiredVersion:      access.Spec.DesiredVersion,
		}); err != nil {
			return err
		}
	}

	for _, change := range plan.Plan.DesiredChanges {
		if err := t.queries.UpdateAccessDesiredState(ctx, db.UpdateAccessDesiredStateParams{
			AccessID:       change.AccessID,
			DesiredState:   string(change.DesiredState),
			DesiredVersion: change.DesiredVersion,
		}); err != nil {
			return err
		}
	}

	for _, change := range plan.Plan.Repoints {
		if err := t.queries.RepointAccess(ctx, db.RepointAccessParams{
			AccessID:       change.AccessID,
			EgressKey:      change.NewEgressKey,
			DesiredState:   string(change.DesiredState),
			DesiredVersion: change.DesiredVersion,
		}); err != nil {
			return err
		}
	}

	for _, spec := range plan.Plan.Retire {
		if err := t.queries.RetireAccess(ctx, db.RetireAccessParams{
			AccessID:       spec.Access.ID,
			DesiredVersion: spec.DesiredVersion,
			ApplyState:     applyStateForRetire(spec.IssueOperation),
		}); err != nil {
			return err
		}
	}

	return nil
}

// writeMaterializedOperations supersede-ит устаревшие операции и кладёт новые в
// outbox.
//
// Supersede выполняется для каждого изменённого access, включая ретайр на
// глобально удалённой ноде: его pending-операции нужно supersede-ить,
// и это единственный способ не оставить в очереди команду на endpoint, которого
// больше нет.
func (t *materializeTx) writeMaterializedOperations(ctx context.Context, plan app.MaterializedManifestPlan) error {
	for _, change := range plan.Plan.DesiredChanges {
		if err := t.supersede(ctx, change.AccessID, change.DesiredVersion); err != nil {
			return err
		}
	}
	for _, change := range plan.Plan.Repoints {
		if err := t.supersede(ctx, change.AccessID, change.DesiredVersion); err != nil {
			return err
		}
	}
	for _, spec := range plan.Plan.Retire {
		if err := t.supersede(ctx, spec.Access.ID, spec.DesiredVersion); err != nil {
			return err
		}
	}

	for _, operation := range plan.Operations {
		operationType, err := operationTypeFor(operation.DesiredState)
		if err != nil {
			return err
		}

		if err := t.queries.InsertAgentOperation(ctx, db.InsertAgentOperationParams{
			OperationID:    operation.OperationID,
			NodeID:         string(operation.NodeID),
			AccessID:       operation.AccessID,
			OperationType:  operationType,
			DesiredVersion: operation.DesiredVersion,
		}); err != nil {
			return err
		}
	}

	return nil
}

func (t *materializeTx) supersede(ctx context.Context, accessID uuid.UUID, desiredVersion int64) error {
	return t.queries.SupersedeStaleOperations(ctx, db.SupersedeStaleOperationsParams{
		AccessID:       accessID,
		DesiredVersion: desiredVersion,
	})
}

// applyStateForRetire — состояние доставки ретайрнутого access.
//
// Живая нода получает EnsureUserAbsent, значит доставка ещё предстоит (PENDING).
// У глобально удалённой ноды операции нет: юзер исчез вместе с её runtime, и
// PENDING означал бы вечно недоставленную операцию в метрике.
func applyStateForRetire(issueOperation bool) string {
	if issueOperation {
		return string(domain.ApplyStatePending)
	}
	return string(domain.ApplyStateApplied)
}
