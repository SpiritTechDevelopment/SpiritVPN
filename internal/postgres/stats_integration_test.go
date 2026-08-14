package postgres

import (
	"context"
	"math"
	"testing"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
)

// Интеграционные тесты снимка состояния для метрик. Смысл слоя целиком в SQL:
// агрегаты, предикаты и касты. Проверять их без базы нечем — юнит-тест здесь
// проверял бы копию запроса, а не запрос.

// collectStats снимает состояние через настоящий адаптер.
func collectStats(t *testing.T, stack expiryStack) app.Stats {
	t.Helper()

	stats, err := New(stack.pool).CollectStats(context.Background())
	if err != nil {
		t.Fatalf("CollectStats: %v", err)
	}
	return stats
}

// accessCount находит счётчик пары состояний. Отсутствие пары — ноль: группировка
// не выдаёт строк там, где access нет.
func accessCount(stats app.Stats, desired, apply string) int64 {
	for _, stat := range stats.Accesses {
		if stat.DesiredState == desired && stat.ApplyState == apply {
			return stat.Count
		}
	}
	return 0
}

func operationCount(stats app.Stats, status string) int64 {
	for _, stat := range stats.Operations {
		if stat.Status == status {
			return stat.Count
		}
	}
	return 0
}

func leaseStat(t *testing.T, stats app.Stats, worker string) app.LeaseStat {
	t.Helper()

	for _, stat := range stats.Leases {
		if stat.Worker == worker {
			return stat
		}
	}
	t.Fatalf("в снимке нет lease воркера %s", worker)
	return app.LeaseStat{}
}

// TestIntegrationStatsReflectDeliveredCustomer — снимок на живом состоянии:
// топология применена, access доставлены, операции завершены.
func TestIntegrationStatsReflectDeliveredCustomer(t *testing.T) {
	stack := newExpiryStack(t)
	seedActiveCustomer(t, stack)

	stats := collectStats(t, stack)

	if stats.ManifestRevision != 7 {
		t.Errorf("ревизия манифеста %d, ожидалась 7", stats.ManifestRevision)
	}
	// Fan-out завершён, поэтому материализованная ревизия догнала принятую и лаг
	// обнулился. Ноль лага при этом означает «незавершённых джоб нет».
	if stats.MaterializedRevision != stats.ManifestRevision {
		t.Errorf("материализована ревизия %d при принятой %d",
			stats.MaterializedRevision, stats.ManifestRevision)
	}
	if stats.MaterializationLagSeconds != 0 {
		t.Errorf("лаг материализации %v, ожидался 0", stats.MaterializationLagSeconds)
	}

	if got := accessCount(stats, "PRESENT", "APPLIED"); got != seedOperationCount {
		t.Errorf("access PRESENT/APPLIED %d, ожидалось %d", got, seedOperationCount)
	}
	if got := operationCount(stats, "SUCCEEDED"); got != seedOperationCount {
		t.Errorf("операций SUCCEEDED %d, ожидалось %d", got, seedOperationCount)
	}

	// Срок не наступил — очередь истечения пуста, и лаг обязан быть нулевым.
	if stats.ExpiredCustomers != 0 || stats.ExpiryLagSeconds != 0 {
		t.Errorf("действующий customer попал в просроченные: %d, лаг %v",
			stats.ExpiredCustomers, stats.ExpiryLagSeconds)
	}
	if stats.ExhaustedNodeQuotas != 0 {
		t.Errorf("исчерпанных квот %d, ожидалось 0", stats.ExhaustedNodeQuotas)
	}

	// Все воркеры отработали и lease сняли.
	for _, worker := range []string{workerDispatch, workerMaterialize, workerUsage} {
		if stat := leaseStat(t, stats, worker); stat.Held != 0 || stat.Expired != 0 {
			t.Errorf("lease воркера %s: занято %d, протухло %d — ожидались нули",
				worker, stat.Held, stat.Expired)
		}
	}

	if stats.SchemaVersion == 0 || stats.SchemaDirty {
		t.Errorf("версия схемы %d, dirty=%v", stats.SchemaVersion, stats.SchemaDirty)
	}
}

// TestIntegrationStatsExpiryLagAppearsAndClears — прямая проверка того, что метрика
// задержки истечения считает ТУ ЖЕ очередь, которую разбирает воркер.
//
// Тест непустой в обе стороны: без предиката EXISTS(PRESENT) второе измерение
// показало бы просроченного customer и после гашения, то есть лаг рос бы вечно
// при исправном воркере. Это ровно та ошибка, которую предикат и закрывает.
func TestIntegrationStatsExpiryLagAppearsAndClears(t *testing.T) {
	stack := newExpiryStack(t)
	seedActiveCustomer(t, stack)
	expireCustomer(t, stack)

	overdue := collectStats(t, stack)
	if overdue.ExpiredCustomers != 1 {
		t.Fatalf("просроченных customer %d, ожидался 1", overdue.ExpiredCustomers)
	}
	if overdue.ExpiryLagSeconds <= 0 {
		t.Errorf("лаг истечения %v, ожидался положительный", overdue.ExpiryLagSeconds)
	}

	drainExpiry(t, stack.expiry)

	cleared := collectStats(t, stack)
	if cleared.ExpiredCustomers != 0 {
		t.Errorf("после гашения просроченных %d, ожидался 0", cleared.ExpiredCustomers)
	}
	if cleared.ExpiryLagSeconds != 0 {
		t.Errorf("после гашения лаг %v, ожидался 0", cleared.ExpiryLagSeconds)
	}

	// Гашение сменило desired_state, но доставку никто не крутил: access ушли в
	// ABSENT/PENDING, и снимок обязан это показать. Без проверки предыдущие
	// сравнения прошли бы и на пустой таблице.
	if got := accessCount(cleared, "ABSENT", "PENDING"); got != seedOperationCount {
		t.Errorf("access ABSENT/PENDING %d, ожидалось %d", got, seedOperationCount)
	}
	if got := operationCount(cleared, "PENDING"); got != seedOperationCount {
		t.Errorf("операций PENDING %d, ожидалось %d", got, seedOperationCount)
	}
}

// cursorStat находит состояние опроса конкретной ноды.
func cursorStat(t *testing.T, stats app.Stats, nodeID string) app.NodeCursorStat {
	t.Helper()

	for _, stat := range stats.Cursors {
		if stat.NodeID == nodeID {
			return stat
		}
	}
	t.Fatalf("ноды %s нет в снимке курсоров (%d строк)", nodeID, len(stats.Cursors))
	return app.NodeCursorStat{}
}

// TestIntegrationStatsReportsNodeCursors — снимок отдаёт состояние опроса по
// каждой текущей ноде.
//
// Проверка acked_sequence на верхней границе uint64 существенна: колонка
// объявлена numeric(20,0) именно потому, что bigint этот диапазон не вмещает, и
// конверсия в снимке идёт тем же хелпером, что и объёмы трафика. Обрежь её int64
// — метрика показала бы отрицательное число или ноль на живом спуле.
func TestIntegrationStatsReportsNodeCursors(t *testing.T) {
	stack := newExpiryStack(t)
	seedActiveCustomer(t, stack)

	// NL-1 опрашивается штатно, у DE-1 повис lease умершего воркера.
	exec(t, stack.pool,
		`INSERT INTO node_usage_cursors (node_id, spool_id, acked_sequence, updated_at)
		 VALUES ('NL-1', 'spool-1', 18446744073709551615::numeric, now() - interval '90 seconds')`)
	exec(t, stack.pool,
		`INSERT INTO node_usage_cursors
		     (node_id, spool_id, acked_sequence, updated_at, lease_owner, lease_expires_at)
		 VALUES ('DE-1', 'spool-2', 7, now() - interval '10 seconds',
		         'умерший-воркер', now() - interval '1 second')`)

	stats := collectStats(t, stack)

	if len(stats.Cursors) != 2 {
		t.Fatalf("строк курсоров %d, ожидалось 2", len(stats.Cursors))
	}

	healthy := cursorStat(t, stats, "NL-1")
	if healthy.AckedSequence != math.MaxUint64 {
		t.Errorf("acked_sequence NL-1 = %d, ожидалось %d: позиция спула обрезана",
			healthy.AckedSequence, uint64(math.MaxUint64))
	}
	if healthy.LastPullAgeSeconds < 60 {
		t.Errorf("возраст опроса NL-1 = %v, ожидался не меньше 60 секунд", healthy.LastPullAgeSeconds)
	}
	if healthy.LeaseExpired {
		t.Error("NL-1 показан с протухшим lease, хотя lease у него не брали")
	}

	stale := cursorStat(t, stats, "DE-1")
	if !stale.LeaseExpired {
		t.Error("DE-1 не показан с протухшим lease: смерть воркера опроса не видна наружу")
	}
	if stale.AckedSequence != 7 {
		t.Errorf("acked_sequence DE-1 = %d, ожидалось 7", stale.AckedSequence)
	}
}

// Нода, снятая с манифеста, из снимка уходит: понодная серия по ней больше не
// обновляется, и оставленная строка навсегда застыла бы на последнем значении.
func TestIntegrationStatsCursorsExcludeRetiredNode(t *testing.T) {
	stack := newExpiryStack(t)
	seedActiveCustomer(t, stack)

	exec(t, stack.pool,
		`INSERT INTO node_usage_cursors (node_id, spool_id, acked_sequence, updated_at)
		 VALUES ('NL-1', 'spool-1', 3, now())`)

	if got := len(collectStats(t, stack).Cursors); got != 1 {
		t.Fatalf("подготовка: строк курсоров %d, ожидалась 1", got)
	}

	exec(t, stack.pool, `UPDATE vpn_nodes SET current = false WHERE node_id = 'NL-1'`)

	if got := len(collectStats(t, stack).Cursors); got != 0 {
		t.Errorf("строк курсоров %d после снятия ноды с манифеста, ожидалось 0", got)
	}
}
