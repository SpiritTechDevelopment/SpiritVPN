package metrics

import (
	"context"
	"fmt"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
)

// Значения метки result у stats_refreshes_total.
const (
	resultOK    = "ok"
	resultError = "error"
)

// StatsWorker переносит снимок состояния БД в gauge (§15).
//
// Отдельный воркер, а не запрос на scrape: иначе стоимость наблюдения задавал бы
// Prometheus снаружи процесса, а недоступная база превращала бы /metrics в
// зависший запрос — то есть отнимала бы метрики ровно тогда, когда они нужны.
//
// Значения при этом устаревают на интервал снимка. Это видно снаружи по
// stats_last_refresh_timestamp_seconds, и alert на его возраст обязателен: без
// него отказ воркера выглядит как застывшие, но правдоподобные числа.
type StatsWorker struct {
	repo app.StatsRepository
	reg  *Registry
	now  func() time.Time
}

// StatsWorker собирает воркер снимка.
func (r *Registry) StatsWorker(repo app.StatsRepository) *StatsWorker {
	return &StatsWorker{repo: repo, reg: r, now: time.Now}
}

// ProcessNext снимает состояние БД один раз.
//
// progressed всегда false, и это не заглушка: цикл runWorker спит idleInterval
// именно на пустом проходе, а снимку нужен ровно фиксированный темп, а не гонка
// «пока есть работа». Второй смысл — «работы больше нет» — здесь совпадает с
// «работа сделана».
func (w *StatsWorker) ProcessNext(ctx context.Context) (bool, error) {
	stats, err := w.repo.CollectStats(ctx)
	if err != nil {
		w.reg.statsRefreshes.WithLabelValues(resultError).Inc()
		return false, fmt.Errorf("снимок состояния для метрик: %w", err)
	}

	w.reg.publish(stats, w.now())
	w.reg.statsRefreshes.WithLabelValues(resultOK).Inc()
	return false, nil
}

// publish раскладывает снимок по gauge.
//
// Векторы с динамическим набором меток сбрасываются перед записью: без этого
// исчезнувшая строка (снятая нода, опустевший статус) осталась бы навсегда
// показывать последнее значение — то есть врала бы именно про то, что пропало.
// Скаляры сбрасывать не нужно, они перезаписываются целиком.
func (r *Registry) publish(stats app.Stats, at time.Time) {
	r.operations.Reset()
	r.operationOldestAge.Reset()
	r.accesses.Reset()
	r.cursorAge.Reset()
	r.cursorAcked.Reset()
	r.cursorLeaseExpired.Reset()
	r.quarantine.Reset()

	// Reset стирает и предзаполнение нулями, поэтому оно повторяется здесь.
	r.seedEnums()

	for _, stat := range stats.Operations {
		r.operations.WithLabelValues(stat.Status).Set(float64(stat.Count))
		r.operationOldestAge.WithLabelValues(stat.Status).Set(stat.OldestAgeSeconds)
	}

	for _, stat := range stats.Accesses {
		r.accesses.WithLabelValues(stat.DesiredState, stat.ApplyState).Set(float64(stat.Count))
	}

	for _, stat := range stats.Cursors {
		r.cursorAge.WithLabelValues(stat.NodeID).Set(stat.LastPullAgeSeconds)
		r.cursorAcked.WithLabelValues(stat.NodeID).Set(float64(stat.AckedSequence))
		r.cursorLeaseExpired.WithLabelValues(stat.NodeID).Set(boolGauge(stat.LeaseExpired))
	}

	for _, stat := range stats.Quarantine {
		r.quarantine.WithLabelValues(stat.Reason).Set(float64(stat.Count))
	}

	for _, stat := range stats.Leases {
		r.leasesHeld.WithLabelValues(stat.Worker).Set(float64(stat.Held))
		r.leasesExpired.WithLabelValues(stat.Worker).Set(float64(stat.Expired))
	}

	r.manifestRevision.Set(float64(stats.ManifestRevision))
	r.materializedRevision.Set(float64(stats.MaterializedRevision))
	r.materializationLag.Set(stats.MaterializationLagSeconds)
	r.expiredCustomers.Set(float64(stats.ExpiredCustomers))
	r.expiryLag.Set(stats.ExpiryLagSeconds)
	r.exhaustedQuotas.Set(float64(stats.ExhaustedNodeQuotas))
	r.schemaVersion.Set(float64(stats.SchemaVersion))
	r.schemaDirty.Set(boolGauge(stats.SchemaDirty))

	r.statsRefreshedAt.Set(float64(at.Unix()))
}
