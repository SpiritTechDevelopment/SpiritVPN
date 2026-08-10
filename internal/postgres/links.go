package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	"github.com/RomanRyabinkin/SpiritVPN/internal/postgres/db"
)

// LoadCustomerLinks читает состояние ссылок customer одним снимком (§5).
//
// Уровень изоляции здесь REPEATABLE READ, а не READ COMMITTED командного пути.
// Причина в том, что снимок собирается двумя операторами: срок действия customer
// приходит из первого, набор его access — из второго, а решение о блокировке
// принимается по обоим сразу. В READ COMMITTED каждый оператор берёт свой снимок,
// и параллельный renewal между ними дал бы ответ, в котором истёкший срок
// сочетается с уже продлёнными access. REPEATABLE READ для read-only транзакции
// стоит ровно ничего: строк она не блокирует, а serialization failure на таком
// уровне PostgreSQL не выдаёт.
//
// §11.1 фиксирует READ COMMITTED для транзакций, меняющих состояние; read-путь он
// не описывает, и это отступлением не является.
func (r *Repository) LoadCustomerLinks(ctx context.Context, customerID string) (app.CustomerLinks, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return app.CustomerLinks{}, fmt.Errorf("postgres: начать read-транзакцию: %w", err)
	}
	// Commit не вызывается намеренно: фиксировать нечего, а rollback освобождает
	// снимок ровно так же и одним оператором.
	defer func() { _ = tx.Rollback(ctx) }()

	queries := db.New(tx)

	header, err := queries.GetCustomerLinksHeader(ctx, customerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.CustomerLinks{}, domain.ErrCustomerNotFound
	}
	if err != nil {
		return app.CustomerLinks{}, fmt.Errorf("чтение entitlement: %w", err)
	}

	rows, err := queries.ListCustomerAccessLinks(ctx, customerID)
	if err != nil {
		return app.CustomerLinks{}, fmt.Errorf("чтение ссылок: %w", err)
	}

	return app.CustomerLinks{
		Now:       header.TxNow,
		ExpiresAt: header.ExpiresAt,
		Accesses:  linkSourcesFromRows(rows),
	}, nil
}

// linkSourcesFromRows сохраняет порядок (kind, logical_target_key, access_id),
// заданный запросом: он же порядок ответа §5.
func linkSourcesFromRows(rows []db.ListCustomerAccessLinksRow) []app.AccessLinkSource {
	sources := make([]app.AccessLinkSource, 0, len(rows))
	for _, row := range rows {
		sources = append(sources, linkSourceFromRow(row))
	}
	return sources
}

// linkSourceFromRow переносит строку в форму, из которой домен выводит состояние.
// Состояние здесь не вычисляется: §5 требует выводить его из сырых фактов, а не
// хранить, и место этого вывода — domain.LinkStatusOf.
func linkSourceFromRow(row db.ListCustomerAccessLinksRow) app.AccessLinkSource {
	source := app.AccessLinkSource{
		Kind:           domain.AccessKind(row.Kind),
		DesiredState:   domain.DesiredState(row.DesiredState),
		ApplyState:     domain.ApplyState(row.ApplyState),
		QuotaExhausted: row.QuotaExhausted,
		Entry:          nodePublicFrom(row.EntryPublicConfig),
		Credential: crypto.SealedCredential{
			KeyID: row.EncryptionKeyID,
			Blob:  row.EncryptedClientUuid,
		},
	}

	// NULL приходит на каждой FREEDOM-строке: display_name связи у неё нет по
	// построению, и пустая строка означает ровно это.
	if row.BridgeDisplayName != nil {
		source.BridgeDisplayName = *row.BridgeDisplayName
	}

	return source
}
