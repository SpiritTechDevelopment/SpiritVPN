package app

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
)

// MaterializeManifest — воркер материализации манифеста.
//
// Раздаёт customer access под топологию, принятую ApplyFleetManifest: новая нода
// даёт FREEDOM, новая связь — BRIDGE, исчезнувшая цель — RETIRED/ABSENT.
// Выполняется вне RPC-транзакции манифеста, потому что 50 000 customer в
// одной транзакции нарушили бы требование о коротких транзакциях.
//
// Сетевых вызовов агентам здесь нет: операции только кладутся в outbox, как и в
// ApplyCustomerAccess.
type MaterializeManifest struct {
	Repo   MaterializationRepository
	IDs    IDs
	Sealer CredentialSealer

	// Owner попадает в lease_owner. Идентифицирует реплику, а не процесс-одиночку:
	// по нему видно, чей lease протух.
	Owner string
	// LeaseTTL — на сколько берётся джоба. Переживший падение lease подбирает
	// следующий воркер, продолжая с курсора.
	LeaseTTL time.Duration
}

// NewMaterializeManifest собирает воркер из портов.
func NewMaterializeManifest(
	repo MaterializationRepository,
	ids IDs,
	sealer CredentialSealer,
	owner string,
	leaseTTL time.Duration,
) *MaterializeManifest {
	return &MaterializeManifest{Repo: repo, IDs: ids, Sealer: sealer, Owner: owner, LeaseTTL: leaseTTL}
}

// ProcessNext выполняет один шаг: берёт джобу и обрабатывает одного customer.
//
// Шаг маленький намеренно. Потолок пакета — 500 строк, но это именно потолок:
// у customer максимум ~100 access (≤10 нод и ≤90 связей на fleet), поэтому один
// customer в него укладывается всегда. Взамен «сначала row
// lock корневой строки» выполняется тривиально, и не нужно упорядочивать
// блокировки нескольких customer внутри одной транзакции.
//
// progressed=false означает, что работы нет: вызывающему цикл стоит подождать.
func (uc *MaterializeManifest) ProcessNext(ctx context.Context) (progressed bool, err error) {
	err = uc.Repo.WithinMaterializationTx(ctx, func(tx MaterializationTx) error {
		job, jobErr := tx.ClaimJob(ctx, uc.Owner, uc.LeaseTTL)
		if jobErr != nil {
			return fmt.Errorf("взятие джобы материализации: %w", jobErr)
		}
		if job == nil {
			return nil
		}

		// Обход идёт по всем customer в порядке customer_id:
		// затронутые fleet из manifest_revision не вычисляются — у удалённых строк
		// остаётся прежняя revision, и удаления таким фильтром не
		// находятся.
		customerID, nextErr := tx.NextCustomer(ctx, job.Cursor)
		if nextErr != nil {
			return fmt.Errorf("выбор следующего customer: %w", nextErr)
		}
		if customerID == "" {
			progressed = true
			return tx.CompleteJob(ctx, job.Revision)
		}

		if err := uc.materializeCustomer(ctx, tx, customerID); err != nil {
			return err
		}

		progressed = true
		// Курсор двигается в той же транзакции, что и изменения customer: прогресс
		// становится ровно-однократным, и падение воркера не требует отдельной
		// сверки.
		return tx.AdvanceCursor(ctx, job.Revision, customerID)
	})
	if err != nil {
		return false, err
	}
	return progressed, nil
}

// materializeCustomer приводит набор access одного customer к текущей топологии.
//
// Порядок чтений совпадает с нормативным порядком блокировок и с порядком
// в ApplyCustomerAccess: entitlement → quota_periods → node_quota_usage, и лишь
// потом не берущие locks чтения топологии и access.
func (uc *MaterializeManifest) materializeCustomer(
	ctx context.Context,
	tx MaterializationTx,
	customerID string,
) error {
	now, err := tx.Now(ctx)
	if err != nil {
		return fmt.Errorf("время транзакции: %w", err)
	}

	entitlement, err := tx.LockEntitlement(ctx, customerID)
	if err != nil {
		return fmt.Errorf("блокировка entitlement: %w", err)
	}
	if entitlement == nil {
		// Customer исчез между выборкой и блокировкой. Удалений в v1 нет,
		// так что это недостижимо, но молча материализовать пустоту хуже, чем
		// пропустить шаг: курсор всё равно сдвинется.
		return nil
	}

	period, err := tx.LockOpenQuotaPeriod(ctx, customerID)
	if err != nil {
		return fmt.Errorf("блокировка периода квоты: %w", err)
	}

	input := domain.MaterializationInput{
		Now:         now,
		Entitlement: *entitlement,
		OpenPeriod:  period,
	}

	if period != nil {
		usage, usageErr := tx.LockNodeQuotaUsage(ctx, period.ID)
		if usageErr != nil {
			return fmt.Errorf("блокировка расхода по нодам: %w", usageErr)
		}
		input.NodeUsage = usage
	}

	if input.Topology, err = tx.LoadTopology(ctx, entitlement.FleetID); err != nil {
		return fmt.Errorf("чтение топологии: %w", err)
	}
	if input.Accesses, err = tx.LoadAccesses(ctx, customerID); err != nil {
		return fmt.Errorf("чтение access: %w", err)
	}
	if input.LiveNodes, err = tx.LoadLiveNodes(ctx); err != nil {
		return fmt.Errorf("чтение живых нод: %w", err)
	}

	plan, err := domain.PlanMaterialize(input)
	if err != nil {
		return err
	}
	// Согласованный customer — самый частый случай при полном обходе: писать
	// нечего, и транзакция не трогает ни одной строки.
	if plan.IsNoOp() {
		return nil
	}

	materialized, err := uc.materializeManifestPlan(customerID, plan, period)
	if err != nil {
		return err
	}

	return tx.WriteMaterialization(ctx, materialized)
}

// materializeManifestPlan дополняет доменный план идентификаторами, credentials и
// операциями — тем, что домен принципиально не производит.
func (uc *MaterializeManifest) materializeManifestPlan(
	customerID string,
	plan domain.MaterializationPlan,
	period *domain.QuotaPeriod,
) (MaterializedManifestPlan, error) {
	materialized := MaterializedManifestPlan{
		CustomerID: customerID,
		PeriodID:   period.ID,
		Plan:       plan,
	}

	for _, spec := range plan.CreateAccesses {
		access, err := uc.newAccessFor(spec)
		if err != nil {
			return MaterializedManifestPlan{}, err
		}
		materialized.NewAccesses = append(materialized.NewAccesses, access)

		// Как и в Apply: родившийся ABSENT операции не получает — отсутствие уже
		// является состоянием Xray по умолчанию.
		if spec.DesiredState != domain.DesiredStatePresent {
			continue
		}
		operation, err := uc.newOperationFor(spec.EntryNodeID, access.AccessID, spec.DesiredState, spec.DesiredVersion)
		if err != nil {
			return MaterializedManifestPlan{}, err
		}
		materialized.Operations = append(materialized.Operations, operation)
	}

	for _, change := range plan.DesiredChanges {
		operation, err := uc.newOperationFor(change.EntryNodeID, change.AccessID, change.DesiredState, change.DesiredVersion)
		if err != nil {
			return MaterializedManifestPlan{}, err
		}
		materialized.Operations = append(materialized.Operations, operation)
	}

	// Repoint доставляется обычной операцией: агент переиздаёт персональное
	// правило по новому egress_key.
	for _, change := range plan.Repoints {
		operation, err := uc.newOperationFor(change.EntryNodeID, change.AccessID, change.DesiredState, change.DesiredVersion)
		if err != nil {
			return MaterializedManifestPlan{}, err
		}
		materialized.Operations = append(materialized.Operations, operation)
	}

	for _, spec := range plan.Retire {
		if !spec.IssueOperation {
			continue
		}
		operation, err := uc.newOperationFor(
			spec.Access.EntryNodeID, spec.Access.ID, domain.DesiredStateAbsent, spec.DesiredVersion)
		if err != nil {
			return MaterializedManifestPlan{}, err
		}
		materialized.Operations = append(materialized.Operations, operation)
	}

	return materialized, nil
}

// newAccessFor выдаёт новой цели идентичность и credential. Каждое поколение
// получает новые client_uuid и accounting_id.
func (uc *MaterializeManifest) newAccessFor(spec domain.NewAccessSpec) (NewAccess, error) {
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

func (uc *MaterializeManifest) newOperationFor(
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
