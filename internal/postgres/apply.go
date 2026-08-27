package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	"github.com/RomanRyabinkin/SpiritVPN/internal/postgres/db"
)

// Значения колонки operation_type. Словарь живёт здесь, а не в домене: домен
// оперирует desired state, а тип операции однозначно из него выводится.
// Значения apply_state, наоборот, домену известны — их читает read-путь.
const (
	operationTypePresent = "ENSURE_PRESENT"
	operationTypeAbsent  = "ENSURE_ABSENT"
)

// Repository — адаптер app.ApplyRepository поверх пула соединений.
type Repository struct {
	pool *pgxpool.Pool
}

// New собирает адаптер. Владелец пула — вызывающий.
func New(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// WithinTx выполняет fn в одной READ COMMITTED транзакции с явными row locks:
// SERIALIZABLE и distributed locks не требуются.
func (r *Repository) WithinTx(ctx context.Context, fn func(app.ApplyTx) error) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("postgres: начать транзакцию: %w", err)
	}
	// Rollback после успешного Commit — no-op, поэтому defer безопасен и закрывает
	// в том числе выход по панике.
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(&applyTx{queries: db.New(tx)}); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit: %w", err)
	}
	return nil
}

func (r *Repository) WithinLifecycleTx(ctx context.Context, fn func(app.LifecycleTx) error) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("postgres: начать lifecycle-транзакцию: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(&applyTx{queries: db.New(tx)}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit lifecycle: %w", err)
	}
	return nil
}

// applyTx реализует шаги одной транзакции ApplyCustomerAccess.
type applyTx struct {
	queries *db.Queries
}

func (t *applyTx) Now(ctx context.Context) (time.Time, error) {
	return t.queries.SelectTransactionNow(ctx)
}

// LockEntitlement сначала заводит DELETED tombstone как стабильную точку
// сериализации. Поэтому PostgreSQL-адаптер возвращает строку и для первой
// команды; nil остаётся допустимым контрактом порта для других адаптеров.
func (t *applyTx) LockEntitlement(ctx context.Context, customerID string) (*domain.Entitlement, error) {
	if err := t.queries.EnsureCustomerEntitlementRoot(ctx, customerID); err != nil {
		return nil, err
	}
	row, err := t.queries.LockCustomerEntitlement(ctx, customerID)
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
	return &entitlement, nil
}

func (t *applyTx) FleetIsCurrent(ctx context.Context, fleetID int64) (bool, error) {
	return t.queries.FleetIsCurrent(ctx, fleetID)
}

// LockOpenQuotaPeriod возвращает nil, если открытого периода нет. Для
// существующего customer это нарушение инварианта, и распознаёт его домен
// (ErrOpenPeriodMissing), а не адаптер.
func (t *applyTx) LockOpenQuotaPeriod(ctx context.Context, customerID string) (*domain.QuotaPeriod, error) {
	row, err := t.queries.LockOpenQuotaPeriod(ctx, customerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	period, err := quotaPeriodFromRow(row)
	if err != nil {
		return nil, err
	}
	return &period, nil
}

func (t *applyTx) LockNodeQuotaUsage(ctx context.Context, periodID uuid.UUID) ([]domain.NodeQuotaUsage, error) {
	rows, err := t.queries.LockNodeQuotaUsage(ctx, periodID)
	if err != nil {
		return nil, err
	}
	return nodeUsageFromRows(rows)
}

func (t *applyTx) LoadTopology(ctx context.Context, fleetID int64) (domain.FleetTopology, error) {
	nodes, err := t.queries.ListCurrentFleetNodes(ctx, fleetID)
	if err != nil {
		return domain.FleetTopology{}, err
	}
	routes, err := t.queries.ListCurrentFleetBridgeRoutes(ctx, fleetID)
	if err != nil {
		return domain.FleetTopology{}, err
	}
	return topologyFromRows(fleetID, nodes, routes), nil
}

func (t *applyTx) LoadLiveNodes(ctx context.Context) ([]domain.NodeID, error) {
	nodes, err := t.queries.ListCurrentNodeIDs(ctx)
	if err != nil {
		return nil, err
	}
	return nodeIDsFromStrings(nodes), nil
}

func (t *applyTx) LoadAccesses(ctx context.Context, customerID string) ([]domain.Access, error) {
	rows, err := t.queries.ListCustomerAccesses(ctx, customerID)
	if err != nil {
		return nil, err
	}
	return accessesFromRows(rows), nil
}

// WritePlan записывает план в нормативном порядке блокировок:
//
//  1. customer_entitlements
//  2. quota_periods
//  3. node_quota_usage
//  5. vpn_nodes
//  6. vpn_accesses
//  7. agent_operations
//
// Шаг 4 (traffic_usage_items_processed) принадлежит usage worker и здесь
// пропускается; пропуск не затрагиваемых строк допустим, менять взаимный порядок
// фактически берущихся locks — нет.
// AppendAudit добавляет запись в append-only журнал.
//
// Живёт на applyTx, а не на каждом пути отдельно: expiryTx встраивает его и
// получает метод даром, а отображение AuditEvent в колонки остаётся в одном
// месте.
func (t *applyTx) AppendAudit(ctx context.Context, event app.AuditEvent) error {
	return appendAudit(ctx, t.queries, event)
}

func (t *applyTx) WritePlan(ctx context.Context, plan app.MaterializedPlan) error {
	if err := t.writeEntitlement(ctx, plan); err != nil {
		return err
	}
	if err := t.writeQuotaPeriod(ctx, plan); err != nil {
		return err
	}
	if err := t.writeNodeQuotaUsage(ctx, plan); err != nil {
		return err
	}
	if err := t.writeTouchedNodes(ctx, plan); err != nil {
		return err
	}
	if err := t.writeAccesses(ctx, plan); err != nil {
		return err
	}
	return t.writeOperations(ctx, plan)
}

// writeEntitlement фиксирует корневую строку. last_command_number двигается всегда,
// включая валидный no-op.
func (t *applyTx) writeEntitlement(ctx context.Context, plan app.MaterializedPlan) error {
	commandNumber := numericFromUint64(plan.CommandNumber)

	if plan.Plan.CreateEntitlement {
		// В PostgreSQL этот путь не используется: LockEntitlement заранее создаёт
		// tombstone и PlanApply выбирает ReactivateEntitlement. Ветка сохраняет
		// общий контракт порта для адаптеров, способных вернуть nil.
		return t.queries.InsertCustomerEntitlement(ctx, db.InsertCustomerEntitlementParams{
			CustomerID:             plan.CustomerID,
			VpnFleetID:             &plan.FleetID,
			ExpiresAt:              &plan.Plan.ExpiresAt,
			DesiredVersion:         plan.Plan.EntitlementDesiredVersion,
			LastCommandNumber:      commandNumber,
			LastCommandFingerprint: plan.CommandFingerprint,
		})
	}
	if plan.Plan.ReactivateEntitlement {
		return t.queries.ReactivateCustomerEntitlement(ctx, db.ReactivateCustomerEntitlementParams{
			CustomerID: plan.CustomerID, VpnFleetID: &plan.FleetID,
			ExpiresAt:              &plan.Plan.ExpiresAt,
			DesiredVersion:         plan.Plan.EntitlementDesiredVersion,
			LastCommandNumber:      commandNumber,
			LastCommandFingerprint: plan.CommandFingerprint,
		})
	}

	return t.queries.UpdateCustomerEntitlement(ctx, db.UpdateCustomerEntitlementParams{
		CustomerID:             plan.CustomerID,
		ExpiresAt:              &plan.Plan.ExpiresAt,
		DesiredVersion:         plan.Plan.EntitlementDesiredVersion,
		LastCommandNumber:      commandNumber,
		LastCommandFingerprint: plan.CommandFingerprint,
	})
}

func (t *applyTx) InsertDeletedTombstone(ctx context.Context, customerID string, commandNumber uint64, fingerprint []byte) error {
	return t.queries.InsertDeletedCustomerTombstone(ctx, db.InsertDeletedCustomerTombstoneParams{
		CustomerID: customerID, LastCommandNumber: numericFromUint64(commandNumber),
		LastCommandFingerprint: fingerprint,
	})
}

func (t *applyTx) WriteLifecycle(ctx context.Context, plan app.MaterializedLifecyclePlan) error {
	if plan.Target == domain.CustomerLifecycleDeleted {
		// Уже завершённый tombstone: shape constraint запрещает delete_not_before.
		plan.DeleteNotBefore = nil
	}
	if err := t.queries.UpdateCustomerLifecycle(ctx, db.UpdateCustomerLifecycleParams{
		LifecycleState: string(plan.Target), DesiredVersion: plan.DesiredVersion,
		LastCommandNumber:      numericFromUint64(plan.CommandNumber),
		LastCommandFingerprint: plan.CommandFingerprint,
		DeleteNotBefore:        plan.DeleteNotBefore, CustomerID: plan.CustomerID,
	}); err != nil {
		return err
	}

	common := app.MaterializedPlan{
		CustomerID: plan.CustomerID,
		Plan:       domain.ApplyPlan{DesiredChanges: plan.DesiredChanges, TouchedNodes: plan.TouchedNodes},
		Operations: plan.Operations,
	}
	if err := t.writeTouchedNodes(ctx, common); err != nil {
		return err
	}
	if err := t.writeAccesses(ctx, common); err != nil {
		return err
	}
	for _, accessID := range plan.AppliedWithoutOperation {
		if err := t.queries.MarkAccessDesiredApplied(ctx, accessID); err != nil {
			return err
		}
	}
	return t.writeOperations(ctx, common)
}

// writeQuotaPeriod закрывает и открывает период при renewal либо меняет лимит
// текущего.
func (t *applyTx) writeQuotaPeriod(ctx context.Context, plan app.MaterializedPlan) error {
	switch {
	case plan.Plan.OpenNewPeriod:
		// closed_at и started_at берут одно и то же now(), поэтому периоды стыкуются
		// без дыры. У нового customer закрывать нечего, и UPDATE просто не
		// найдёт строк.
		if err := t.queries.CloseOpenQuotaPeriod(ctx, plan.CustomerID); err != nil {
			return err
		}
		return t.queries.InsertQuotaPeriod(ctx, db.InsertQuotaPeriodParams{
			QuotaPeriodID:   plan.PeriodID,
			CustomerID:      plan.CustomerID,
			UsageQuotaBytes: numericFromUint64(plan.Plan.QuotaBytes),
		})

	case plan.Plan.UpdatePeriodQuota:
		return t.queries.UpdateQuotaPeriodQuota(ctx, db.UpdateQuotaPeriodQuotaParams{
			QuotaPeriodID:   plan.PeriodID,
			UsageQuotaBytes: numericFromUint64(plan.Plan.QuotaBytes),
		})

	default:
		return nil
	}
}

// writeNodeQuotaUsage заводит нулевые строки нового периода и пересчитывает отметки
// исчерпания в текущем. Списки отсортированы по node_id.
func (t *applyTx) writeNodeQuotaUsage(ctx context.Context, plan app.MaterializedPlan) error {
	if len(plan.Plan.NodeQuotaInits) > 0 {
		if err := t.queries.InsertNodeQuotaUsage(ctx, db.InsertNodeQuotaUsageParams{
			QuotaPeriodID: plan.PeriodID,
			NodeIds:       nodeIDStrings(plan.Plan.NodeQuotaInits),
		}); err != nil {
			return err
		}
	}

	for _, change := range plan.Plan.NodeQuotaChanges {
		if err := t.queries.SetNodeQuotaExhausted(ctx, db.SetNodeQuotaExhaustedParams{
			QuotaPeriodID: plan.PeriodID,
			NodeID:        string(change.NodeID),
			ExhaustedAt:   change.ExhaustedAt,
		}); err != nil {
			return err
		}
	}

	return nil
}

// writeTouchedNodes блокирует ноды в порядке node_id и ровно один раз увеличивает
// каждой desired_revision независимо от числа затронутых access.
func (t *applyTx) writeTouchedNodes(ctx context.Context, plan app.MaterializedPlan) error {
	if len(plan.Plan.TouchedNodes) == 0 {
		return nil
	}

	nodeIDs := nodeIDStrings(plan.Plan.TouchedNodes)

	locked, err := t.queries.LockNodesForUpdate(ctx, nodeIDs)
	if err != nil {
		return err
	}
	// Нода из текущей топологии существует в vpn_nodes: manifest
	// валидируется на существование всех node references. Расхождение означает
	// рассогласованную проекцию, и продолжать с ним нельзя — FK всё равно уронит
	// вставку access, но уже без внятной причины.
	if len(locked) != len(nodeIDs) {
		return fmt.Errorf("postgres: заблокировано %d нод из %d — проекция топологии рассогласована",
			len(locked), len(nodeIDs))
	}

	return t.queries.BumpNodesDesiredRevision(ctx, nodeIDs)
}

// writeAccesses создаёт недостающие access и переводит существующие в новое desired
// state. Оба списка отсортированы по нормативному порядку блокировок.
func (t *applyTx) writeAccesses(ctx context.Context, plan app.MaterializedPlan) error {
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

	return nil
}

// writeOperations кладёт операции в outbox.
func (t *applyTx) writeOperations(ctx context.Context, plan app.MaterializedPlan) error {
	// Устаревшие операции есть только у существующих access: у только что созданных
	// прошлых версий не бывает. Порядок — по access_id, как отсортирован план.
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

// operationTypeFor выводит тип операции из desired state.
func operationTypeFor(state domain.DesiredState) (string, error) {
	switch state {
	case domain.DesiredStatePresent:
		return operationTypePresent, nil
	case domain.DesiredStateAbsent:
		return operationTypeAbsent, nil
	default:
		return "", fmt.Errorf("postgres: неизвестный desired_state %q", state)
	}
}

// applyStateForNewAccess — состояние доставки только что созданного access.
// ABSENT рождается APPLIED: операции у него нет, а отсутствие юзера
// уже является состоянием Xray по умолчанию, и PENDING означал бы вечно висящую
// недоставленную операцию в метрике.
func applyStateForNewAccess(state domain.DesiredState) string {
	if state == domain.DesiredStatePresent {
		return string(domain.ApplyStatePending)
	}
	return string(domain.ApplyStateApplied)
}
