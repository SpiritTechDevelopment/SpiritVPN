package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	"github.com/RomanRyabinkin/SpiritVPN/internal/nodeagent"
	"github.com/RomanRyabinkin/SpiritVPN/internal/postgres/db"
)

// ClaimNode берёт lease ноды, которую пора опросить.
//
// Строки курсора заводятся тем же вызовом: claim берёт строку под
// FOR UPDATE SKIP LOCKED, а несуществующую заблокировать нельзя. Вставка
// идемпотентна и почти всегда не делает ничего.
func (r *Repository) ClaimNode(
	ctx context.Context,
	owner string,
	leaseTTL, minInterval time.Duration,
) (*app.ClaimedUsageNode, error) {
	leaseSeconds, err := leaseSeconds(leaseTTL)
	if err != nil {
		return nil, err
	}
	intervalSeconds := int32(minInterval.Seconds())
	if intervalSeconds < 0 {
		return nil, fmt.Errorf("postgres: недопустимый интервал опроса %s", minInterval)
	}

	queries := db.New(r.pool)
	if err := queries.EnsureUsageCursors(ctx); err != nil {
		return nil, err
	}

	row, err := queries.ClaimUsageNode(ctx, db.ClaimUsageNodeParams{
		Owner:              owner,
		LeaseSeconds:       leaseSeconds,
		MinIntervalSeconds: intervalSeconds,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	acked, err := uint64FromNumeric(row.AckedSequence)
	if err != nil {
		return nil, fmt.Errorf("node_usage_cursors.acked_sequence: %w", err)
	}

	return &app.ClaimedUsageNode{
		NodeID:   domain.NodeID(row.NodeID),
		Endpoint: nodeAgentFrom(row.NodeID, row.AgentConfig),
		Cursor:   nodeagent.UsageCursor{SpoolID: row.SpoolID, Sequence: acked},
	}, nil
}

func (r *Repository) ReleaseNode(ctx context.Context, nodeID domain.NodeID, owner string) error {
	return db.New(r.pool).ReleaseUsageLease(ctx, db.ReleaseUsageLeaseParams{
		NodeID: string(nodeID),
		Owner:  owner,
	})
}

// ResolveAccounting строит исторический маппинг accounting_id → владелец.
func (r *Repository) ResolveAccounting(
	ctx context.Context,
	accountingIDs []string,
) (map[string]app.UsageOwner, error) {
	if len(accountingIDs) == 0 {
		return nil, nil
	}

	rows, err := db.New(r.pool).ResolveAccountingIDs(ctx, accountingIDs)
	if err != nil {
		return nil, err
	}

	owners := make(map[string]app.UsageOwner, len(rows))
	for _, row := range rows {
		owners[row.AccountingID] = app.UsageOwner{
			AccessID:    row.AccessID,
			CustomerID:  row.CustomerID,
			EntryNodeID: domain.NodeID(row.EntryNodeID),
		}
	}
	return owners, nil
}

// QuarantineItems кладёт items в карантин и отмечает их обработанными.
//
// Обе записи в одной транзакции: карантинная строка без отметки processed
// означала бы, что item приедет снова и попадёт в карантин повторно.
func (r *Repository) QuarantineItems(
	ctx context.Context,
	batch app.UsageBatchRef,
	reason string,
	items []domain.UsageItem,
) error {
	if len(items) == 0 {
		return nil
	}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("postgres: начать транзакцию карантина: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	queries := db.New(tx)
	sequence := numericFromUint64(batch.Sequence)

	accountingIDs := make([]string, 0, len(items))
	uplinks := make([]pgtype.Numeric, 0, len(items))
	downlinks := make([]pgtype.Numeric, 0, len(items))
	for _, item := range items {
		accountingIDs = append(accountingIDs, item.AccountingID)
		uplinks = append(uplinks, numericFromUint64(item.UplinkBytes))
		downlinks = append(downlinks, numericFromUint64(item.DownlinkBytes))
	}

	if err := queries.QuarantineUsageItems(ctx, db.QuarantineUsageItemsParams{
		NodeID:        string(batch.NodeID),
		SpoolID:       batch.SpoolID,
		Sequence:      sequence,
		Reason:        reason,
		AccountingIds: accountingIDs,
		Uplinks:       uplinks,
		Downlinks:     downlinks,
	}); err != nil {
		return err
	}

	if err := queries.RegisterQuarantinedUsageItems(ctx, db.RegisterQuarantinedUsageItemsParams{
		NodeID:        string(batch.NodeID),
		SpoolID:       batch.SpoolID,
		Sequence:      sequence,
		AccountingIds: accountingIDs,
	}); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit карантина: %w", err)
	}
	return nil
}

func (r *Repository) AdvanceCursor(
	ctx context.Context,
	nodeID domain.NodeID,
	cursor nodeagent.UsageCursor,
) error {
	return db.New(r.pool).AdvanceUsageCursor(ctx, db.AdvanceUsageCursorParams{
		NodeID:        string(nodeID),
		SpoolID:       cursor.SpoolID,
		AckedSequence: numericFromUint64(cursor.Sequence),
	})
}

// PruneProcessedUsageItems удаляет одну пачку дедуп-записей (ретенция).
//
// Вне транзакции: удаление пачки и есть вся работа шага. Объединённые пачки
// держали бы длинную транзакцию ровно на той таблице, которая растёт быстрее
// всех.
func (r *Repository) PruneProcessedUsageItems(
	ctx context.Context,
	retention time.Duration,
	limit int,
) (int, error) {
	deleted, err := db.New(r.pool).PruneProcessedUsageItems(ctx, db.PruneProcessedUsageItemsParams{
		RetentionSeconds: retention.Seconds(),
		MaxRows:          int64(limit),
	})
	if err != nil {
		return 0, fmt.Errorf("postgres: очистка реестра дедупа: %w", err)
	}
	return int(deleted), nil
}

// WithinUsageGroupTx выполняет обработку одной группы в одной транзакции.
func (r *Repository) WithinUsageGroupTx(ctx context.Context, fn func(app.UsageGroupTx) error) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("postgres: начать транзакцию группы usage: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(&usageGroupTx{applyTx: applyTx{queries: db.New(tx)}}); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit группы usage: %w", err)
	}
	return nil
}

// usageGroupTx встраивает applyTx ради Now и LockEntitlement — они те же, что и у
// командного пути. Свои методы ниже — только то, чего у него нет.
type usageGroupTx struct {
	applyTx
}

// LockPeriodAt возвращает nil, когда подходящего периода нет.
func (t *usageGroupTx) LockPeriodAt(
	ctx context.Context,
	customerID string,
	collectedAt time.Time,
) (*app.UsagePeriod, error) {
	row, err := t.queries.LockQuotaPeriodAt(ctx, db.LockQuotaPeriodAtParams{
		CustomerID:  customerID,
		CollectedAt: collectedAt,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	period, err := quotaPeriodFromRow(db.QuotaPeriod{
		QuotaPeriodID:   row.QuotaPeriodID,
		CustomerID:      row.CustomerID,
		StartedAt:       row.StartedAt,
		ClosedAt:        row.ClosedAt,
		UsageQuotaBytes: row.UsageQuotaBytes,
	})
	if err != nil {
		return nil, err
	}

	return &app.UsagePeriod{Period: period, Closed: row.ClosedAt != nil}, nil
}

// LockNodeUsage заводит строку расхода при отсутствии и берёт её FOR UPDATE.
//
// Заводит, потому что трафик мог прийти по ноде, которой строки не досталось:
// истёкшему customer материализация их не создаёт, а начислять его items
// всё равно нужно. Без строки байты просто потерялись бы.
func (t *usageGroupTx) LockNodeUsage(
	ctx context.Context,
	periodID uuid.UUID,
	nodeID domain.NodeID,
) (domain.NodeQuotaUsage, error) {
	if err := t.queries.EnsureNodeQuotaUsageRow(ctx, db.EnsureNodeQuotaUsageRowParams{
		QuotaPeriodID: periodID,
		NodeID:        string(nodeID),
	}); err != nil {
		return domain.NodeQuotaUsage{}, err
	}

	row, err := t.queries.LockNodeQuotaUsageRow(ctx, db.LockNodeQuotaUsageRowParams{
		QuotaPeriodID: periodID,
		NodeID:        string(nodeID),
	})
	if err != nil {
		return domain.NodeQuotaUsage{}, err
	}

	total, err := uint64FromNumeric(row.TotalBytes)
	if err != nil {
		return domain.NodeQuotaUsage{}, fmt.Errorf("node_quota_usage.total_bytes: %w", err)
	}

	return domain.NodeQuotaUsage{
		NodeID:      domain.NodeID(row.NodeID),
		TotalBytes:  total,
		ExhaustedAt: row.ExhaustedAt,
	}, nil
}

// RegisterProcessed возвращает только те items, которых в реестре ещё не было.
func (t *usageGroupTx) RegisterProcessed(
	ctx context.Context,
	batch app.UsageBatchRef,
	periodID uuid.UUID,
	result domain.UsageItemResult,
	items []domain.UsageItem,
) ([]domain.UsageItem, error) {
	if len(items) == 0 {
		return nil, nil
	}

	accountingIDs := make([]string, 0, len(items))
	accessIDs := make([]uuid.UUID, 0, len(items))
	for _, item := range items {
		accountingIDs = append(accountingIDs, item.AccountingID)
		accessIDs = append(accessIDs, item.AccessID)
	}

	inserted, err := t.queries.RegisterProcessedUsageItems(ctx, db.RegisterProcessedUsageItemsParams{
		NodeID:        string(batch.NodeID),
		SpoolID:       batch.SpoolID,
		Sequence:      numericFromUint64(batch.Sequence),
		QuotaPeriodID: periodID,
		Result:        string(result),
		AccountingIds: accountingIDs,
		AccessIds:     accessIDs,
	})
	if err != nil {
		return nil, err
	}
	if len(inserted) == len(items) {
		return items, nil
	}

	fresh := make(map[string]struct{}, len(inserted))
	for _, accountingID := range inserted {
		fresh[accountingID] = struct{}{}
	}

	// Порядок items сохраняется: он задан доменной группировкой, а перестановка
	// здесь сделала бы план зависимым от порядка строк RETURNING.
	accrued := make([]domain.UsageItem, 0, len(inserted))
	for _, item := range items {
		if _, ok := fresh[item.AccountingID]; ok {
			accrued = append(accrued, item)
		}
	}
	return accrued, nil
}

func (t *usageGroupTx) LoadNodeAccesses(
	ctx context.Context,
	customerID string,
	nodeID domain.NodeID,
) ([]domain.Access, error) {
	rows, err := t.queries.ListCustomerNodeAccesses(ctx, db.ListCustomerNodeAccessesParams{
		CustomerID:  customerID,
		EntryNodeID: string(nodeID),
	})
	if err != nil {
		return nil, err
	}

	accesses := make([]domain.Access, 0, len(rows))
	for _, row := range rows {
		accesses = append(accesses, accessFromRow(db.ListCustomerAccessesRow(row)))
	}
	return accesses, nil
}

// WriteUsageGroup записывает план в нормативном порядке блокировок:
//
//  3. node_quota_usage — начисление и отметка исчерпания
//  5. vpn_nodes
//  6. vpn_accesses
//  7. agent_operations
//
// Шаги 1, 2 и 4 уже выполнены вызывающим: корневая строка, период и реестр
// идемпотентности блокируются до планирования.
func (t *usageGroupTx) WriteUsageGroup(ctx context.Context, plan app.MaterializedUsageGroup) error {
	accrual := plan.Plan.Accrual
	if len(accrual.Items) > 0 {
		if err := t.queries.AddNodeQuotaUsage(ctx, db.AddNodeQuotaUsageParams{
			QuotaPeriodID: plan.PeriodID,
			NodeID:        string(plan.NodeID),
			UplinkBytes:   numericFromUint64(accrual.UplinkBytes),
			DownlinkBytes: numericFromUint64(accrual.DownlinkBytes),
		}); err != nil {
			return err
		}
	}

	if plan.Plan.ExhaustedAt != nil {
		if err := t.queries.SetNodeQuotaExhausted(ctx, db.SetNodeQuotaExhaustedParams{
			QuotaPeriodID: plan.PeriodID,
			NodeID:        string(plan.NodeID),
			ExhaustedAt:   plan.Plan.ExhaustedAt,
		}); err != nil {
			return err
		}
	}

	if len(plan.Plan.DesiredChanges) == 0 {
		return nil
	}

	// Затронута ровно одна нода — та, чья квота исчерпана. desired_revision растёт
	// один раз, сколько бы access на ней ни гасло.
	nodeIDs := []string{string(plan.NodeID)}
	locked, err := t.queries.LockNodesForUpdate(ctx, nodeIDs)
	if err != nil {
		return err
	}
	if len(locked) != 1 {
		return fmt.Errorf("postgres: нода %s не найдена — проекция топологии рассогласована", plan.NodeID)
	}
	if err := t.queries.BumpNodesDesiredRevision(ctx, nodeIDs); err != nil {
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

	// Supersede по той же причине, что и у expiry: у гасимого access мог висеть
	// недоставленный EnsureUserPresent прежней версии.
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

// Компиляторная проверка, что адаптер закрывает порт целиком.
var _ app.UsageRepository = (*Repository)(nil)
