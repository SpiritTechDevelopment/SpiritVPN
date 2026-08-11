package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	"github.com/RomanRyabinkin/SpiritVPN/internal/postgres/db"
)

// ReapExpiredLeases возвращает в оборот операции умерших воркеров (§9).
//
// Собственной транзакции не открывает: это один оператор, и неявная транзакция
// pgx его полностью покрывает.
func (r *Repository) ReapExpiredLeases(ctx context.Context, maxReaped int32) (int64, error) {
	if maxReaped < 1 {
		return 0, fmt.Errorf("postgres: недопустимый предел сбора lease %d", maxReaped)
	}
	return db.New(r.pool).ReapExpiredOperationLeases(ctx, maxReaped)
}

// LeaseNext берёт lease готовой операции и собирает payload (§9). nil означает,
// что отправлять нечего.
func (r *Repository) LeaseNext(
	ctx context.Context,
	owner string,
	leaseTTL time.Duration,
) (*app.LeasedOperation, error) {
	seconds, err := leaseSeconds(leaseTTL)
	if err != nil {
		return nil, err
	}

	row, err := db.New(r.pool).LeaseNextOperation(ctx, db.LeaseNextOperationParams{
		Owner:        owner,
		LeaseSeconds: seconds,
	})
	// Пустой результат и проигранная гонка за ноду означают для вызывающего одно и
	// то же: отправлять сейчас нечего. Гонка возможна, потому что гейт в запросе
	// читает committed-снимок, а инвариант §9 держит partial unique index — см.
	// agent_operations_single_in_flight_per_node. Проигравший не ошибка, а
	// нормальный исход конкуренции восьми воркеров (решение 39).
	if errors.Is(err, pgx.ErrNoRows) || isSingleInFlightViolation(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	desiredState, err := desiredStateFor(row.OperationType)
	if err != nil {
		return nil, err
	}

	return &app.LeasedOperation{
		OperationID:          row.OperationID,
		AccessID:             row.AccessID,
		DesiredVersion:       row.DesiredVersion,
		AttemptCount:         row.AttemptCount,
		DesiredState:         desiredState,
		AccessDesiredVersion: row.AccessDesiredVersion,
		Endpoint:             nodeAgentFrom(row.NodeID, row.AgentConfig),
		AccountingID:         row.AccountingID,
		EgressKey:            row.EgressKey,
		Flow:                 nodePublicFrom(row.PublicConfig).Flow,
		Credential: crypto.SealedCredential{
			Blob:  row.EncryptedClientUuid,
			KeyID: row.EncryptionKeyID,
		},
	}, nil
}

// WithinResultTx выполняет запись исхода в одной READ COMMITTED транзакции.
//
// Транзакция открывается уже ПОСЛЕ возврата от агента: §11.1 требует, чтобы во
// время обращения к node-agent не оставалось ни одной открытой транзакции.
func (r *Repository) WithinResultTx(ctx context.Context, fn func(app.ResultTx) error) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("postgres: начать транзакцию результата: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(&resultTx{queries: db.New(tx)}); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit результата: %w", err)
	}
	return nil
}

// resultTx — шаги транзакции записи исхода доставки.
type resultTx struct {
	queries *db.Queries
}

func (t *resultTx) Now(ctx context.Context) (time.Time, error) {
	return t.queries.SelectTransactionNow(ctx)
}

// SetAccessApplyState возвращает false, когда desired_version строки уже ушла
// вперёд и UPDATE не нашёл её (§11.1).
func (t *resultTx) SetAccessApplyState(
	ctx context.Context,
	accessID uuid.UUID,
	desiredVersion int64,
	state domain.ApplyState,
) (bool, error) {
	affected, err := t.queries.SetAccessApplyState(ctx, db.SetAccessApplyStateParams{
		AccessID:       accessID,
		DesiredVersion: desiredVersion,
		ApplyState:     string(state),
	})
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func (t *resultTx) CompleteOperation(ctx context.Context, result app.OperationResult) error {
	return t.queries.CompleteOperation(ctx, db.CompleteOperationParams{
		OperationID:   result.OperationID,
		Status:        string(result.Status),
		NextAttemptAt: result.NextAttemptAt,
		Completed:     result.Completed,
		ErrorCode:     result.ErrorCode,
		ErrorMessage:  result.ErrorMessage,
	})
}

const (
	// singleInFlightIndex — индекс, держащий инвариант «одна операция на ноду».
	singleInFlightIndex = "agent_operations_single_in_flight_per_node"

	// uniqueViolation — SQLSTATE 23505. Литералом, а не через отдельный модуль
	// pgerrcode: ради одной константы зависимость не заводим.
	uniqueViolation = "23505"
)

// isSingleInFlightViolation опознаёт проигранную гонку за ноду.
//
// Проверяется именно этот индекс, а не любой unique violation: коллизия
// accounting_id или повторная (access_id, desired_version) — нарушение инварианта,
// которое обязано провалить шаг громко, а не быть проглоченным как «занято»
// (решение 4).
func isSingleInFlightViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == uniqueViolation &&
		pgErr.ConstraintName == singleInFlightIndex
}

// desiredStateFor — обратная operationTypeFor конверсия. Словарь значений колонки
// operation_type не покидает этот пакет ни при записи, ни при чтении (§9).
func desiredStateFor(operationType string) (domain.DesiredState, error) {
	switch operationType {
	case operationTypePresent:
		return domain.DesiredStatePresent, nil
	case operationTypeAbsent:
		return domain.DesiredStateAbsent, nil
	default:
		return "", fmt.Errorf("postgres: неизвестный operation_type %q", operationType)
	}
}

// leaseSeconds сужает TTL под тип колонки, проверяя границы ДО приведения:
// отрицательный или абсурдно большой TTL дал бы бессмысленный lease вместо явной
// ошибки. Тот же приём, что и в ClaimJob воркера материализации.
func leaseSeconds(leaseTTL time.Duration) (int32, error) {
	seconds := leaseTTL.Seconds()
	if seconds < 1 || seconds > math.MaxInt32 {
		return 0, fmt.Errorf("postgres: недопустимый lease TTL %s", leaseTTL)
	}
	return int32(seconds), nil
}

// Компиляторная проверка, что адаптер закрывает порт целиком.
var _ app.DispatchRepository = (*Repository)(nil)
