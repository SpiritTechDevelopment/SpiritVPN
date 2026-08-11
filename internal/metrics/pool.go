package metrics

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// RegisterPool публикует состояние пула соединений (§15).
//
// Коллектор, а не gauge, которые кто-то обновляет: pgxpool.Stat уже снимок, он
// стоит одну блокировку и никуда не ходит. Копировать его в отдельные переменные
// на интервале значило бы завести второй источник тех же чисел и отвечать на
// вопрос «почему в метрике не то, что в пуле».
//
// Поэтому же он не участвует в снимке БД: походом в базу здесь и не пахнет,
// и на scrape это ровно так же дёшево.
func (r *Registry) RegisterPool(pool *pgxpool.Pool) {
	r.reg.MustRegister(&poolCollector{pool: pool})
}

// Описания метрик пула. Собираются один раз: prometheus.Desc неизменяем, а
// пересоздавать его на каждый Collect — лишний мусор на каждый scrape.
var (
	poolConnsDesc = prometheus.NewDesc(
		namespace+"_db_pool_connections",
		"PostgreSQL pool connections, by state.",
		[]string{"state"}, nil,
	)
	poolMaxConnsDesc = prometheus.NewDesc(
		namespace+"_db_pool_max_connections",
		"Maximum size of the PostgreSQL connection pool.",
		nil, nil,
	)
	poolAcquireDesc = prometheus.NewDesc(
		namespace+"_db_pool_acquires_total",
		"Connection acquisitions from the pool, by outcome.",
		[]string{labelResult}, nil,
	)
	poolAcquireWaitDesc = prometheus.NewDesc(
		namespace+"_db_pool_acquire_wait_seconds_total",
		"Total time spent waiting to acquire a connection from the pool.",
		nil, nil,
	)
)

type poolCollector struct {
	pool *pgxpool.Pool
}

func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- poolConnsDesc
	ch <- poolMaxConnsDesc
	ch <- poolAcquireDesc
	ch <- poolAcquireWaitDesc
}

func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	stat := c.pool.Stat()

	gauge := func(value float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(poolConnsDesc, prometheus.GaugeValue, value, labels...)
	}
	gauge(float64(stat.TotalConns()), "total")
	gauge(float64(stat.IdleConns()), "idle")
	gauge(float64(stat.AcquiredConns()), "acquired")
	gauge(float64(stat.ConstructingConns()), "constructing")

	ch <- prometheus.MustNewConstMetric(poolMaxConnsDesc, prometheus.GaugeValue, float64(stat.MaxConns()))

	// empty и canceled — не подмножества total, а независимые счётчики pgxpool:
	// первый считает захваты, которым пришлось ждать свободного соединения,
	// второй — отменённые контекстом. Разделение по метке, а не по трём метрикам,
	// чтобы исчерпание пула читалось одним отношением.
	ch <- prometheus.MustNewConstMetric(
		poolAcquireDesc, prometheus.CounterValue, float64(stat.AcquireCount()), "total")
	ch <- prometheus.MustNewConstMetric(
		poolAcquireDesc, prometheus.CounterValue, float64(stat.EmptyAcquireCount()), "empty")
	ch <- prometheus.MustNewConstMetric(
		poolAcquireDesc, prometheus.CounterValue, float64(stat.CanceledAcquireCount()), "canceled")

	ch <- prometheus.MustNewConstMetric(
		poolAcquireWaitDesc, prometheus.CounterValue, stat.EmptyAcquireWaitTime().Seconds())
}
