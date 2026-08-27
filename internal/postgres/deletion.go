package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/RomanRyabinkin/SpiritVPN/internal/postgres/db"
)

// FinalizeNextDeletion физически очищает одного сошедшегося DELETING customer.
// Вся очистка и перевод tombstone атомарны; падение процесса оставляет либо всё,
// либо ничего и следующий worker безопасно повторяет шаг.
func (r *Repository) FinalizeNextDeletion(ctx context.Context) (bool, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, fmt.Errorf("postgres: начать cleanup deletion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := db.New(tx)
	row, err := q.LockNextDeletionCandidate(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	ready, err := q.CustomerDeletionReady(ctx, row.CustomerID)
	if err != nil {
		return false, err
	}
	if ready == nil || !*ready {
		// Кандидат был, но ещё не сошёлся: это не progress, иначе worker крутился
		// бы на одной недоступной ноде без паузы.
		return false, nil
	}
	for _, remove := range []func(context.Context, string) error{
		q.DeleteCustomerQuarantine,
		q.DeleteCustomerProcessedUsage,
		q.DeleteCustomerAgentOperations,
		q.DeleteCustomerNodeUsage,
		q.DeleteCustomerQuotaPeriods,
		q.DeleteCustomerAccesses,
	} {
		if err := remove(ctx, row.CustomerID); err != nil {
			return false, err
		}
	}
	if err := q.FinalizeCustomerTombstone(ctx, row.CustomerID); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("postgres: commit cleanup deletion: %w", err)
	}
	return true, nil
}
