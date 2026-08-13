package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	"github.com/RomanRyabinkin/SpiritVPN/internal/nodeagent"
)

// maxUsageBatchesPerPull — сколько batch забирается за один шаг.
//
// Потолок нужен, чтобы шаг оставался коротким: batch содержит до 5 000 items,
// и неограниченный ответ мог бы обрабатываться дольше собственного lease.
// Недобранное приедет следующим шагом — курсор двигается по каждому batch.
const maxUsageBatchesPerPull = 16

// UsageAgent — то, что pull worker требует от агента.
//
// Порт отдельный от AgentDispatcher: у чтения нет строки в agent_operations, и
// сводить его исход к AttemptOutcome было бы натяжкой.
type UsageAgent interface {
	GetNodeState(
		ctx context.Context,
		endpoint nodeagent.Endpoint,
		acknowledged nodeagent.UsageCursor,
		maxBatches uint32,
	) nodeagent.PullOutcome
}

// PullUsage — воркер учёта трафика.
//
// Единственный, кто наполняет node_quota_usage, и потому единственный, из-за кого
// квота вообще может сработать. Агент в PostgreSQL не пишет: backend тянет у него
// дельты и начисляет их сам.
//
// Шаг состоит из нескольких транзакций с сетевым вызовом между первой и
// остальными: держать транзакцию открытой запрещено во время обращения к
// агенту.
type PullUsage struct {
	Repo   UsageRepository
	Agent  UsageAgent
	IDs    IDs
	Logger *slog.Logger

	// Owner попадает в lease_owner: по нему видно, чей lease протух.
	Owner string
	// LeaseTTL должен с запасом перекрывать RPC вместе со всеми транзакциями
	// групп, иначе вторая реплика начнёт опрашивать ту же ноду параллельно.
	LeaseTTL time.Duration
	// MinInterval — как часто опрашивается одна нода. Чаще, чем агент опрашивает
	// Xray, дёргать нечего: он вернёт пустой ответ.
	MinInterval time.Duration
}

// NewPullUsage собирает воркер из портов.
func NewPullUsage(
	repo UsageRepository,
	agent UsageAgent,
	ids IDs,
	logger *slog.Logger,
	owner string,
	leaseTTL, minInterval time.Duration,
) *PullUsage {
	return &PullUsage{
		Repo:        repo,
		Agent:       agent,
		IDs:         ids,
		Logger:      logger,
		Owner:       owner,
		LeaseTTL:    leaseTTL,
		MinInterval: minInterval,
	}
}

// ProcessNext опрашивает ОДНУ ноду.
//
// progressed=false означает, что опрашивать сейчас нечего. Недоступная нода
// прогрессом считается: работа была проделана, и повторять её немедленно не надо —
// её темп задаёт MinInterval, а не цикл воркера.
func (uc *PullUsage) ProcessNext(ctx context.Context) (progressed bool, err error) {
	node, err := uc.Repo.ClaimNode(ctx, uc.Owner, uc.LeaseTTL, uc.MinInterval)
	if err != nil {
		return false, fmt.Errorf("взятие ноды в опрос: %w", err)
	}
	if node == nil {
		return false, nil
	}

	// Lease снимается на любом выходе, включая отказ: иначе нода простаивала бы до
	// истечения TTL, который заметно длиннее интервала опроса.
	defer uc.release(ctx, node.NodeID)

	outcome := uc.Agent.GetNodeState(ctx, node.Endpoint, node.Cursor, maxUsageBatchesPerPull)
	if !outcome.OK() {
		// Недоступность ноды — не отказ шага: она не меняет ни
		// состава fleet, ни desired state. Записывать нечего, ретрай произойдёт
		// сам собой на следующем интервале.
		uc.logPullFailure(ctx, node.NodeID, outcome)
		return true, nil
	}

	// Признак bootstrap запоминается здесь, потому что этот вызов — единственный
	// источник, из которого он вообще приходит. Реагирует на него reconcile:
	// агент с новым или повреждённым локальным состоянием не имеет права ничего
	// удалять и сам из этого состояния не выйдет.
	if err := uc.Repo.SetNodeNeedsBootstrap(ctx, node.NodeID, outcome.State.NeedsBootstrap); err != nil {
		return true, fmt.Errorf("признак bootstrap ноды: %w", err)
	}

	return true, uc.consume(ctx, *node, outcome.State)
}

// consume обрабатывает batch'и ноды по одному, подтверждая каждый отдельно.
func (uc *PullUsage) consume(ctx context.Context, node ClaimedUsageNode, state *nodeagent.NodeState) error {
	cursor := node.Cursor

	for _, batch := range state.Batches {
		// Смена spool_id означает новый спул: нумерация начинается с нуля, и
		// сравнивать её с прежним acked_sequence нельзя.
		if batch.Cursor.SpoolID != cursor.SpoolID {
			uc.logSpoolChange(ctx, node.NodeID, cursor, batch.Cursor)
			cursor = nodeagent.UsageCursor{SpoolID: batch.Cursor.SpoolID}
		}

		// Монотонность: уже подтверждённый batch — идемпотентный no-op.
		// Дедуп по items сработал бы и без этого, но лишних транзакций не будет.
		if batch.Cursor.Sequence <= cursor.Sequence {
			continue
		}

		if err := uc.consumeBatch(ctx, node.NodeID, batch); err != nil {
			// Курсор не двигаем: batch приедет снова, а уже начисленные items
			// схлопнутся дедупом в no-op.
			return err
		}

		cursor = batch.Cursor
		if err := uc.Repo.AdvanceCursor(ctx, node.NodeID, cursor); err != nil {
			return fmt.Errorf("подтверждение курсора: %w", err)
		}
	}

	return nil
}

// consumeBatch раскладывает один batch по группам и обрабатывает каждую своей
// короткой транзакцией.
func (uc *PullUsage) consumeBatch(ctx context.Context, nodeID domain.NodeID, batch nodeagent.UsageBatch) error {
	ref := UsageBatchRef{NodeID: nodeID, SpoolID: batch.Cursor.SpoolID, Sequence: batch.Cursor.Sequence}

	known, unknown, err := uc.resolve(ctx, nodeID, batch)
	if err != nil {
		return err
	}

	if len(unknown) > 0 {
		// Карантин отдельной транзакцией и ПЕРВЫМ: он ничего не блокирует, а
		// упавшая следом группа не должна оставлять плохие items неотмеченными.
		if err := uc.Repo.QuarantineItems(ctx, ref, domain.QuarantineUnknownAccountingID, unknown); err != nil {
			return fmt.Errorf("карантин неизвестных accounting_id: %w", err)
		}
	}

	for _, group := range domain.GroupUsageItems(known) {
		if err := uc.consumeGroup(ctx, ref, group, batch.CollectedAt); err != nil {
			return err
		}
	}

	return nil
}

// resolve делит items batch на опознанные и карантинные.
func (uc *PullUsage) resolve(
	ctx context.Context,
	nodeID domain.NodeID,
	batch nodeagent.UsageBatch,
) (known, unknown []domain.UsageItem, err error) {
	accountingIDs := make([]string, 0, len(batch.Items))
	for _, item := range batch.Items {
		accountingIDs = append(accountingIDs, item.AccountingID)
	}

	owners, err := uc.Repo.ResolveAccounting(ctx, accountingIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("сопоставление accounting_id: %w", err)
	}

	for _, item := range batch.Items {
		usage := domain.UsageItem{
			AccountingID:  item.AccountingID,
			UplinkBytes:   item.UplinkBytes,
			DownlinkBytes: item.DownlinkBytes,
		}

		owner, ok := owners[item.AccountingID]
		// Чужая нода — такая же аномалия, как неизвестный accounting_id: агент
		// отчитался за юзера, которого backend на нём не размещал. Начислять это
		// на опрошенную ноду нельзя, её квота тут ни при чём.
		if !ok || owner.EntryNodeID != nodeID {
			unknown = append(unknown, usage)
			continue
		}

		usage.CustomerID = owner.CustomerID
		usage.AccessID = owner.AccessID
		usage.EntryNodeID = owner.EntryNodeID
		known = append(known, usage)
	}

	return known, unknown, nil
}

// consumeGroup обрабатывает одну группу (customer_id, node_id, quota_period_id) в
// одной транзакции.
func (uc *PullUsage) consumeGroup(
	ctx context.Context,
	ref UsageBatchRef,
	group domain.UsageGroup,
	collectedAt time.Time,
) error {
	var quarantine []domain.UsageItem

	err := uc.Repo.WithinUsageGroupTx(ctx, func(tx UsageGroupTx) error {
		now, err := tx.Now(ctx)
		if err != nil {
			return fmt.Errorf("время транзакции: %w", err)
		}

		entitlement, err := tx.LockEntitlement(ctx, group.Key.CustomerID)
		if err != nil {
			return fmt.Errorf("блокировка entitlement: %w", err)
		}
		if entitlement == nil {
			// Access есть, а корневой строки нет. Удалений в v1 не бывает,
			// поэтому это рассогласование, а не гонка.
			quarantine = group.Items
			return nil
		}

		period, err := tx.LockPeriodAt(ctx, group.Key.CustomerID, collectedAt)
		if err != nil {
			return fmt.Errorf("блокировка периода: %w", err)
		}
		if period == nil {
			// Подходящего периода нет вовсе — не «закрыт», а не существует.
			// Начислять некуда, и молча терять трафик нельзя.
			quarantine = group.Items
			return nil
		}

		usage, err := tx.LockNodeUsage(ctx, period.Period.ID, group.Key.NodeID)
		if err != nil {
			return fmt.Errorf("блокировка расхода ноды: %w", err)
		}

		result := domain.UsageItemApplied
		if period.Closed {
			result = domain.UsageItemIgnoredClosedPeriod
		}

		fresh, err := tx.RegisterProcessed(ctx, ref, period.Period.ID, result, group.Items)
		if err != nil {
			return fmt.Errorf("регистрация items: %w", err)
		}
		if len(fresh) == 0 {
			// Весь batch этой группы уже начислен: повторный pull, перезапуск
			// воркера или повтор batch. Ровно то, ради чего дедуп и нужен.
			return nil
		}

		input := domain.UsageGroupInput{
			Now:             now,
			Items:           fresh,
			PeriodClosed:    period.Closed,
			QuotaBytes:      period.Period.UsageQuotaBytes,
			NodeTotalBytes:  usage.TotalBytes,
			NodeExhaustedAt: usage.ExhaustedAt,
		}

		// Access читаются только когда порог в принципе может сработать: у
		// закрытого периода и у уже отмеченной ноды они не понадобятся.
		if !period.Closed && usage.ExhaustedAt == nil {
			if input.NodeAccesses, err = tx.LoadNodeAccesses(ctx, group.Key.CustomerID, group.Key.NodeID); err != nil {
				return fmt.Errorf("чтение access ноды: %w", err)
			}
		}

		plan := domain.PlanUsageGroup(input)
		if plan.IsNoOp() {
			return nil
		}

		materialized, err := uc.materializeGroup(group, period.Period.ID, plan)
		if err != nil {
			return err
		}
		return tx.WriteUsageGroup(ctx, materialized)
	})
	if err != nil {
		return err
	}

	if len(quarantine) > 0 {
		return uc.Repo.QuarantineItems(ctx, ref, domain.QuarantineNoQuotaPeriod, quarantine)
	}
	return nil
}

// materializeGroup выдаёт погашенным квотой access операции удаления.
func (uc *PullUsage) materializeGroup(
	group domain.UsageGroup,
	periodID uuid.UUID,
	plan domain.UsageGroupPlan,
) (MaterializedUsageGroup, error) {
	materialized := MaterializedUsageGroup{
		CustomerID: group.Key.CustomerID,
		NodeID:     group.Key.NodeID,
		PeriodID:   periodID,
		Plan:       plan,
	}

	for _, change := range plan.DesiredChanges {
		operationID, err := uc.IDs.NewOperationID()
		if err != nil {
			return MaterializedUsageGroup{}, err
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

// release снимает lease, не заслоняя основную ошибку шага.
func (uc *PullUsage) release(ctx context.Context, nodeID domain.NodeID) {
	// Контекст мог быть отменён вместе с процессом. Тогда снимать lease нечем и
	// незачем: он протухнет сам, и ноду подберёт следующая реплика.
	if ctx.Err() != nil {
		return
	}
	if err := uc.Repo.ReleaseNode(ctx, nodeID, uc.Owner); err != nil {
		uc.Logger.LogAttrs(ctx, slog.LevelWarn, "не удалось снять lease опроса",
			slog.String("node_id", string(nodeID)),
			slog.Any("error", err))
	}
}

func (uc *PullUsage) logPullFailure(ctx context.Context, nodeID domain.NodeID, outcome nodeagent.PullOutcome) {
	level := slog.LevelWarn
	if outcome.Alert {
		level = slog.LevelError
	}

	uc.Logger.LogAttrs(ctx, level, "опрос ноды не удался",
		slog.String("node_id", string(nodeID)),
		slog.String("code", outcome.Code),
		slog.String("message", outcome.Message))
}

// logSpoolChange — потеря спула. Это принятая погрешность учёта, но
// молчать о ней нельзя: она означает безвозвратно потерянный трафик.
func (uc *PullUsage) logSpoolChange(
	ctx context.Context,
	nodeID domain.NodeID,
	was, now nodeagent.UsageCursor,
) {
	if was.SpoolID == "" {
		// Первый в жизни ноды pull, терять было нечего.
		return
	}

	uc.Logger.LogAttrs(ctx, slog.LevelError, "спул ноды сменился, часть трафика не учтена",
		slog.String("node_id", string(nodeID)),
		slog.String("previous_spool_id", was.SpoolID),
		slog.Uint64("previous_acked_sequence", was.Sequence),
		slog.String("spool_id", now.SpoolID))
}
