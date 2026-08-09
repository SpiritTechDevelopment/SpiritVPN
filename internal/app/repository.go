package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
)

// ApplyRepository открывает короткую транзакцию под одну команду customer.
//
// Транзакцией владеет адаптер (он знает про READ COMMITTED, commit и rollback), а
// порядок шагов внутри неё задаёт use case: именно порядок, а не отдельные запросы,
// является нормативным требованием §11.1, и держать его в SQL-слое значило бы
// сделать его непроверяемым без базы.
type ApplyRepository interface {
	// WithinTx выполняет fn в одной транзакции: nil коммитит, ошибка откатывает.
	WithinTx(ctx context.Context, fn func(ApplyTx) error) error
}

// ApplyTx — шаги одной транзакции ApplyCustomerAccess в порядке блокировок §11.1.
//
// Методы разделены по строкам, которые они блокируют, чтобы порядок вызовов в use
// case читался как нормативный список §11.1: entitlement → quota_periods →
// node_quota_usage → vpn_nodes → vpn_accesses → agent_operations. Последние три
// группы записываются одним WritePlan: они пишутся всегда вместе, и разбивать их
// значило бы дать вызывающему возможность нарушить порядок.
type ApplyTx interface {
	// Now — время начала транзакции (SELECT now()). Первый оператор транзакции;
	// единственный источник времени для доменных решений (решение 2, §11.1).
	Now(ctx context.Context) (time.Time, error)

	// LockEntitlement берёт SELECT ... FOR UPDATE корневой строки. nil означает
	// нового customer: строки ещё нет, и её создаст WritePlan.
	LockEntitlement(ctx context.Context, customerID string) (*domain.Entitlement, error)

	// FleetIsCurrent сообщает, присутствует ли fleet в последнем принятом
	// снапшоте manifest (§5, правило 6).
	FleetIsCurrent(ctx context.Context, fleetID int64) (bool, error)

	// LockOpenQuotaPeriod берёт FOR UPDATE единственный период с
	// closed_at IS NULL. nil означает, что открытого периода нет.
	LockOpenQuotaPeriod(ctx context.Context, customerID string) (*domain.QuotaPeriod, error)

	// LockNodeQuotaUsage берёт FOR UPDATE строки расхода периода в порядке node_id.
	LockNodeQuotaUsage(ctx context.Context, periodID uuid.UUID) ([]domain.NodeQuotaUsage, error)

	// LoadTopology читает текущую проекцию топологии fleet. Row locks не берёт.
	LoadTopology(ctx context.Context, fleetID int64) (domain.FleetTopology, error)

	// LoadAccesses читает ВСЕ access customer, включая ретайрнутые (§4). Row locks
	// не берёт: конкурирующий writer невозможен под lock корневой строки.
	LoadAccesses(ctx context.Context, customerID string) ([]domain.Access, error)

	// WritePlan записывает план целиком в порядке блокировок §11.1 и двигает
	// last_command_number. Вызывается и на пустом плане: валидный no-op номер
	// команды всё равно двигает (§5, правило 4).
	WritePlan(ctx context.Context, plan MaterializedPlan) error
}

// MaterializedPlan — доменный план, дополненный тем, что домен принципиально не
// производит: идентификаторами, accounting ID и зашифрованными credentials.
//
// Домен возвращает diff и остаётся детерминированным; недетерминированную часть
// добавляет use case из портов IDs и CredentialSealer.
type MaterializedPlan struct {
	CustomerID string
	// FleetID нужен только при создании корневой строки: смена fleet существующего
	// customer запрещена и отсекается доменом (§5, правило 5).
	FleetID int64
	// CommandNumber попадает в last_command_number при успешном commit.
	CommandNumber uint64

	Plan domain.ApplyPlan

	// PeriodID — период, в который пишутся изменения квоты: новый при
	// Plan.OpenNewPeriod, иначе уже открытый. uuid.Nil допустим только на плане,
	// который периода не касается.
	PeriodID uuid.UUID

	// NewAccesses соответствует Plan.CreateAccesses один к одному и в том же
	// порядке.
	NewAccesses []NewAccess

	// Operations — записи transactional outbox (§9). Операция выпускается только по
	// фактическому изменению desired-кортежа; новый ABSENT-access операции не
	// получает (решение 3.2).
	Operations []AgentOperation
}

// NewAccess — создаваемый access вместе со своей идентичностью и credential.
type NewAccess struct {
	Spec         domain.NewAccessSpec
	AccessID     uuid.UUID
	AccountingID string
	// Credential — зашифрованный client_uuid; открытое значение в план не попадает
	// и живёт только внутри вызова Seal (§7).
	Credential crypto.SealedCredential
}

// AgentOperation — операция агенту в outbox (§9).
//
// Тип операции здесь не хранится строкой: он однозначно выводится из DesiredState
// (PRESENT → EnsureUserPresent, ABSENT → EnsureUserAbsent), и словарь значений
// колонки operation_type остаётся внутри адаптера postgres.
type AgentOperation struct {
	OperationID    uuid.UUID
	NodeID         domain.NodeID
	AccessID       uuid.UUID
	DesiredState   domain.DesiredState
	DesiredVersion int64
}
