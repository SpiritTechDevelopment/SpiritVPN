package postgres

import (
	"context"
	"testing"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
)

// Интеграционные тесты снимка состояния для метрик §15. Смысл слоя целиком в SQL:
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
// при исправном воркере. Это ровно та ошибка, которую предикат и закрывает
// (решение 55).
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
