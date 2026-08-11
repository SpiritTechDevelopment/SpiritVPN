package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	"github.com/RomanRyabinkin/SpiritVPN/internal/postgres/db"
)

// WithinExpiryTx выполняет один шаг expiry worker в одной транзакции (§13).
//
// READ COMMITTED с явными row locks, как и остальные пути: шаг меняет состояние
// одного customer и подчиняется тому же порядку блокировок §11.1.
func (r *Repository) WithinExpiryTx(ctx context.Context, fn func(app.ExpiryTx) error) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("postgres: начать транзакцию истечения: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(&expiryTx{applyTx: applyTx{queries: db.New(tx)}}); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit истечения: %w", err)
	}
	return nil
}

// expiryTx встраивает applyTx ради Now и LoadAccesses — они у обоих путей
// буквально одни и те же. Тот же приём, что и у materializeTx: вторая реализация
// со временем разъехалась бы с первой.
//
// LockEntitlement здесь НЕ используется: воркер не знает customer заранее и
// выбирает его сам, блокируя корневую строку той же выборкой.
type expiryTx struct {
	applyTx
}

// LockNextDueCustomer возвращает nil, когда гасить некого.
func (t *expiryTx) LockNextDueCustomer(ctx context.Context) (*app.ExpiredCustomer, error) {
	row, err := t.queries.LockNextDueExpiredCustomer(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	entitlement, err := entitlementFromRow(row)
	if err != nil {
		return nil, err
	}
	return &app.ExpiredCustomer{CustomerID: row.CustomerID, Entitlement: entitlement}, nil
}

// WriteExpiry записывает план в нормативном порядке блокировок §11.1:
//
//  5. vpn_nodes
//  6. vpn_accesses
//  7. agent_operations
//
// Шаги 1–3 пропущены: корневая строка уже заблокирована выборкой due customer, а
// quota_periods и node_quota_usage истечение не трогает вовсе — desired state
// истёкшего customer равен ABSENT независимо от расхода (решение 56).
func (t *expiryTx) WriteExpiry(ctx context.Context, plan app.MaterializedExpiryPlan) error {
	if err := t.queries.BumpEntitlementDesiredVersion(ctx, db.BumpEntitlementDesiredVersionParams{
		CustomerID:     plan.CustomerID,
		DesiredVersion: plan.Plan.EntitlementDesiredVersion,
	}); err != nil {
		return err
	}

	if err := t.writeExpiredNodes(ctx, plan.Plan.TouchedNodes); err != nil {
		return err
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

	return t.writeExpiryOperations(ctx, plan)
}

// writeExpiredNodes блокирует ноды в порядке node_id и ровно один раз увеличивает
// каждой desired_revision, сколько бы access customer на ней ни было (§11.1).
func (t *expiryTx) writeExpiredNodes(ctx context.Context, nodes []domain.NodeID) error {
	if len(nodes) == 0 {
		return nil
	}

	nodeIDs := nodeIDStrings(nodes)

	locked, err := t.queries.LockNodesForUpdate(ctx, nodeIDs)
	if err != nil {
		return err
	}
	// Нода, на которой стоит живой access, обязана существовать в vpn_nodes:
	// строки оттуда не удаляются никогда, исчезнувшие лишь помечаются
	// current=false (§6). Расхождение означает рассогласованную проекцию.
	if len(locked) != len(nodeIDs) {
		return fmt.Errorf("postgres: заблокировано %d нод из %d — проекция топологии рассогласована",
			len(locked), len(nodeIDs))
	}

	return t.queries.BumpNodesDesiredRevision(ctx, nodeIDs)
}

// writeExpiryOperations supersede-ит устаревшие операции и кладёт Remove в outbox.
//
// Supersede обязателен: у access мог висеть недоставленный EnsureUserPresent
// прежней версии, и без него на ноду уехала бы команда, ставящая юзера обратно
// (§9).
func (t *expiryTx) writeExpiryOperations(ctx context.Context, plan app.MaterializedExpiryPlan) error {
	for _, change := range plan.Plan.DesiredChanges {
		if err := t.queries.SupersedeStaleOperations(ctx, db.SupersedeStaleOperationsParams{
			AccessID:       change.AccessID,
			DesiredVersion: change.DesiredVersion,
		}); err != nil {
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

// AppendAudit — тот же запрос, что и у приёма манифеста (§15).
func (t *expiryTx) AppendAudit(ctx context.Context, event app.AuditEvent) error {
	return appendAudit(ctx, t.queries, event)
}

// Компиляторная проверка, что адаптер закрывает порт целиком.
var _ app.ExpiryRepository = (*Repository)(nil)
