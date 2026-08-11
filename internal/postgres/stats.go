package postgres

import (
	"context"
	"fmt"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/postgres/db"
)

// Имена воркеров в метке метрики lease. Совпадают со значениями, которые
// runWorker пишет в поле worker записи об отказе: метрика и лог должны сходиться
// по одному значению, иначе alert указывает на воркер, которого нет в логах.
//
// Expiry в списке отсутствует: он обходится row lock'ом вместо lease (решение 54).
const (
	workerDispatch    = "dispatch"
	workerMaterialize = "materialize"
	workerUsage       = "usage"
)

// CollectStats снимает состояние БД для метрик §15.
//
// Транзакции нет намеренно, в отличие от LoadCustomerLinks: там снимок собирался
// двумя операторами, и решение принималось по обоим сразу, поэтому расхождение
// между ними было бы ошибкой ответа. Здесь каждое значение уезжает в собственный
// независимый gauge, сравнивать их между собой никто не будет, и REPEATABLE READ
// купил бы только согласованность, которая устареет раньше следующего scrape.
//
// Частичный отказ не маскируется: снимок с молча пропущенной частью выглядел бы
// как «метрика упала до нуля» — то есть врал бы ровно в ту сторону, в которую
// смотрят alert'ы.
func (r *Repository) CollectStats(ctx context.Context) (app.Stats, error) {
	queries := db.New(r.pool)

	operations, err := queries.StatsAgentOperations(ctx)
	if err != nil {
		return app.Stats{}, fmt.Errorf("postgres: статистика операций: %w", err)
	}

	accesses, err := queries.StatsAccesses(ctx)
	if err != nil {
		return app.Stats{}, fmt.Errorf("postgres: статистика access: %w", err)
	}

	cursors, err := queries.StatsNodeCursors(ctx)
	if err != nil {
		return app.Stats{}, fmt.Errorf("postgres: статистика курсоров: %w", err)
	}

	quarantine, err := queries.StatsQuarantine(ctx)
	if err != nil {
		return app.Stats{}, fmt.Errorf("postgres: статистика карантина: %w", err)
	}

	scalars, err := queries.StatsScalars(ctx)
	if err != nil {
		return app.Stats{}, fmt.Errorf("postgres: скалярная статистика: %w", err)
	}

	stats := app.Stats{
		Operations:                operationStatsFromRows(operations),
		Accesses:                  accessStatsFromRows(accesses),
		Quarantine:                quarantineStatsFromRows(quarantine),
		Leases:                    leaseStatsFromRow(scalars),
		ManifestRevision:          scalars.ManifestRevision,
		MaterializedRevision:      scalars.MaterializedRevision,
		MaterializationLagSeconds: scalars.MaterializationLagSeconds,
		ExpiredCustomers:          scalars.ExpiredCustomers,
		ExpiryLagSeconds:          scalars.ExpiryLagSeconds,
		ExhaustedNodeQuotas:       scalars.ExhaustedNodeQuotas,
		UsageDedupOldestAge:       scalars.UsageDedupOldestAgeSeconds,
	}

	if stats.Cursors, err = cursorStatsFromRows(cursors); err != nil {
		return app.Stats{}, err
	}

	if stats.SchemaVersion, stats.SchemaDirty, err = r.schemaVersion(ctx); err != nil {
		return app.Stats{}, err
	}

	return stats, nil
}

// schemaVersion читает версию схемы сырым запросом.
//
// Не через sqlc, потому что schema_migrations заводит golang-migrate: в
// internal/migrations этой таблицы нет, и генератор о ней не знает. Тот же приём
// и по той же причине уже применяет readiness-проверка.
func (r *Repository) schemaVersion(ctx context.Context) (version int64, dirty bool, err error) {
	err = r.pool.QueryRow(ctx, "SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty)
	if err != nil {
		return 0, false, fmt.Errorf("postgres: версия схемы: %w", err)
	}
	return version, dirty, nil
}

func operationStatsFromRows(rows []db.StatsAgentOperationsRow) []app.OperationStat {
	stats := make([]app.OperationStat, 0, len(rows))
	for _, row := range rows {
		stats = append(stats, app.OperationStat{
			Status:           row.Status,
			Count:            row.Count,
			OldestAgeSeconds: row.OldestAgeSeconds,
		})
	}
	return stats
}

func accessStatsFromRows(rows []db.StatsAccessesRow) []app.AccessStat {
	stats := make([]app.AccessStat, 0, len(rows))
	for _, row := range rows {
		stats = append(stats, app.AccessStat{
			DesiredState: row.DesiredState,
			ApplyState:   row.ApplyState,
			Count:        row.Count,
		})
	}
	return stats
}

func quarantineStatsFromRows(rows []db.StatsQuarantineRow) []app.QuarantineStat {
	stats := make([]app.QuarantineStat, 0, len(rows))
	for _, row := range rows {
		stats = append(stats, app.QuarantineStat{Reason: row.Reason, Count: row.Count})
	}
	return stats
}

func cursorStatsFromRows(rows []db.StatsNodeCursorsRow) ([]app.NodeCursorStat, error) {
	stats := make([]app.NodeCursorStat, 0, len(rows))
	for _, row := range rows {
		// acked_sequence — numeric(20,0): int64 обрезал бы верхнюю половину
		// диапазона, поэтому конверсия идёт тем же хелпером, что и объёмы трафика.
		sequence, err := uint64FromNumeric(row.AckedSequence)
		if err != nil {
			return nil, fmt.Errorf("postgres: acked_sequence ноды %s: %w", row.NodeID, err)
		}

		stats = append(stats, app.NodeCursorStat{
			NodeID:             row.NodeID,
			LastPullAgeSeconds: row.LastPullAgeSeconds,
			AckedSequence:      sequence,
			LeaseExpired:       row.LeaseExpired,
		})
	}
	return stats, nil
}

func leaseStatsFromRow(row db.StatsScalarsRow) []app.LeaseStat {
	return []app.LeaseStat{
		{Worker: workerDispatch, Held: row.DispatchLeasesHeld, Expired: row.DispatchLeasesExpired},
		{Worker: workerMaterialize, Held: row.MaterializeLeasesHeld, Expired: row.MaterializeLeasesExpired},
		{Worker: workerUsage, Held: row.UsageLeasesHeld, Expired: row.UsageLeasesExpired},
	}
}
