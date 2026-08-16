package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	"github.com/RomanRyabinkin/SpiritVPN/internal/postgres/db"
)

// ClaimNodeForReconcile берёт ноду и её полный набор одной транзакцией.
//
// Именно одной: desired_revision фиксируется и users читаются под блокировкой
// строки ноды, иначе зафиксированная ревизия описывала бы не тот набор, который
// уехал агенту. Захват берёт строку эксклюзивно, а не в share mode: он всё равно
// её пишет, назначая lease, и вторая блокировка той же строки ничего бы не
// добавила.
//
// Транзакция закрывается до расшифрования и до сетевого вызова.
func (r *Repository) ClaimNodeForReconcile(
	ctx context.Context,
	owner string,
	leaseTTL, minInterval time.Duration,
) (*app.ClaimedReconcileNode, error) {
	leaseSeconds, err := leaseSeconds(leaseTTL)
	if err != nil {
		return nil, err
	}
	intervalSeconds := int32(minInterval.Seconds())
	if intervalSeconds < 0 {
		return nil, fmt.Errorf("postgres: недопустимый интервал reconcile %s", minInterval)
	}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("postgres: начать транзакцию reconcile: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	queries := db.New(tx)

	row, err := queries.ClaimNodeForReconcile(ctx, db.ClaimNodeForReconcileParams{
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

	users, err := queries.ListNodeDesiredUsers(ctx, row.NodeID)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: commit захвата reconcile: %w", err)
	}

	claimed := &app.ClaimedReconcileNode{
		NodeID:          domain.NodeID(row.NodeID),
		Endpoint:        nodeAgentFrom(row.NodeID, row.AgentConfig),
		Flow:            nodePublicFrom(row.PublicConfig).Flow,
		DesiredRevision: row.DesiredRevision,
		NeedsBootstrap:  row.NeedsBootstrap,
		Users:           make([]app.ReconcileUser, 0, len(users)),
	}

	for _, user := range users {
		claimed.Users = append(claimed.Users, app.ReconcileUser{
			AccessID:     user.AccessID,
			AccountingID: user.AccountingID,
			EgressKey:    user.EgressKey,
			Credential: crypto.SealedCredential{
				Blob:  user.EncryptedClientUuid,
				KeyID: user.EncryptionKeyID,
			},
		})
	}

	return claimed, nil
}

func (r *Repository) ReleaseNodeReconcile(ctx context.Context, nodeID domain.NodeID, owner string) error {
	return db.New(r.pool).ReleaseNodeReconcile(ctx, db.ReleaseNodeReconcileParams{
		NodeID: string(nodeID),
		Owner:  owner,
	})
}

// AcceptReconcile применяет результат, если desired_revision не сдвинулась.
//
// Все три записи в одной транзакции: принять набор, но не отметить access
// применёнными означало бы, что следующий проход считает ноду reconcile-нутой, а
// диспетчер продолжит досылать то, что уже доставлено.
func (r *Repository) AcceptReconcile(
	ctx context.Context,
	acceptance app.ReconcileAcceptance,
) (bool, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, fmt.Errorf("postgres: начать транзакцию приёма reconcile: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	queries := db.New(tx)

	affected, err := queries.AcceptNodeReconcile(ctx, db.AcceptNodeReconcileParams{
		NodeID:          string(acceptance.NodeID),
		DesiredRevision: acceptance.DesiredRevision,
	})
	if err != nil {
		return false, err
	}
	if affected == 0 {
		// desired_revision ушла вперёд: набор на проводе устарел. Ничего не
		// записано, транзакция откатывается пустой.
		return false, nil
	}

	applied := acceptance.AppliedAccessIDs
	if applied == nil {
		// Пустой набор легален и означает, что на ноде не должно быть ни
		// одного backend-owned юзера. ANY(NULL) не равен ANY пустого массива,
		// поэтому nil здесь пришлось бы отдельным случаем в SQL.
		applied = []uuid.UUID{}
	}

	if err := queries.MarkNodeAccessesApplied(ctx, db.MarkNodeAccessesAppliedParams{
		NodeID:     string(acceptance.NodeID),
		AppliedIds: applied,
	}); err != nil {
		return false, err
	}

	if err := queries.CompleteNodeOperationsByReconcile(ctx, db.CompleteNodeOperationsByReconcileParams{
		NodeID:    string(acceptance.NodeID),
		ErrorCode: app.CodeReconcileApplied,
	}); err != nil {
		return false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("postgres: commit приёма reconcile: %w", err)
	}
	return true, nil
}

// SetNodeNeedsBootstrap запоминает признак, присланный агентом.
func (r *Repository) SetNodeNeedsBootstrap(
	ctx context.Context,
	nodeID domain.NodeID,
	needsBootstrap bool,
) error {
	return db.New(r.pool).SetNodeNeedsBootstrap(ctx, db.SetNodeNeedsBootstrapParams{
		NodeID:         string(nodeID),
		NeedsBootstrap: needsBootstrap,
	})
}
