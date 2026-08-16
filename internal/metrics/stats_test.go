package metrics

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
)

// sampleStats — снимок со всеми видами динамических меток. Умышленно неполный по
// enum'ам: статусов операций здесь два из шести, и это проверяет, что остальные
// четыре остаются нулями, а не исчезают.
func sampleStats() app.Stats {
	return app.Stats{
		Operations: []app.OperationStat{
			{Status: "PENDING", Count: 3, OldestAgeSeconds: 42},
			{Status: "SUCCEEDED", Count: 17},
		},
		Accesses: []app.AccessStat{
			{DesiredState: "PRESENT", ApplyState: "APPLIED", Count: 100},
			{DesiredState: "PRESENT", ApplyState: "RETRYING", Count: 2},
		},
		Cursors: []app.NodeCursorStat{
			{NodeID: testNodeID, LastPullAgeSeconds: 7, AckedSequence: 512, LeaseExpired: true},
		},
		Quarantine: []app.QuarantineStat{
			{Reason: "UNKNOWN_ACCOUNTING_ID", Count: 5},
		},
		Leases: []app.LeaseStat{
			{Worker: "dispatch", Held: 1, Expired: 0},
			{Worker: "materialize", Held: 0, Expired: 0},
			{Worker: "usage", Held: 2, Expired: 1},
		},
		ManifestRevision:          9,
		MaterializedRevision:      8,
		MaterializationLagSeconds: 31,
		ExpiredCustomers:          4,
		ExpiryLagSeconds:          12,
		ExhaustedNodeQuotas:       1,
		SchemaVersion:             1,
		SchemaDirty:               false,
	}
}

// fakeStatsRepo — снимок или отказ.
type fakeStatsRepo struct {
	stats app.Stats
	err   error
	calls int
}

func (f *fakeStatsRepo) CollectStats(context.Context) (app.Stats, error) {
	f.calls++
	if f.err != nil {
		return app.Stats{}, f.err
	}
	return f.stats, nil
}

func TestPublishSetsGauges(t *testing.T) {
	registry := New()
	registry.publish(sampleStats(), sampleTime)

	for _, tc := range []struct {
		name string
		got  float64
		want float64
	}{
		{"operations PENDING", testutil.ToFloat64(registry.operations.WithLabelValues("PENDING")), 3},
		{"возраст PENDING", testutil.ToFloat64(registry.operationOldestAge.WithLabelValues("PENDING")), 42},
		{"access PRESENT/APPLIED", testutil.ToFloat64(registry.accesses.WithLabelValues("PRESENT", "APPLIED")), 100},
		{"возраст опроса", testutil.ToFloat64(registry.cursorAge.WithLabelValues(testNodeID)), 7},
		{"позиция спула", testutil.ToFloat64(registry.cursorAcked.WithLabelValues(testNodeID)), 512},
		{"протухший lease ноды", testutil.ToFloat64(registry.cursorLeaseExpired.WithLabelValues(testNodeID)), 1},
		{"карантин", testutil.ToFloat64(registry.quarantine.WithLabelValues("UNKNOWN_ACCOUNTING_ID")), 5},
		{"lease usage", testutil.ToFloat64(registry.leasesExpired.WithLabelValues("usage")), 1},
		{"ревизия манифеста", testutil.ToFloat64(registry.manifestRevision), 9},
		{"лаг материализации", testutil.ToFloat64(registry.materializationLag), 31},
		{"лаг истечения", testutil.ToFloat64(registry.expiryLag), 12},
		{"исчерпанные квоты", testutil.ToFloat64(registry.exhaustedQuotas), 1},
		{"схема не dirty", testutil.ToFloat64(registry.schemaDirty), 0},
		{"момент снимка", testutil.ToFloat64(registry.statsRefreshedAt), float64(sampleTime.Unix())},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: %v, ожидалось %v", tc.name, tc.got, tc.want)
		}
	}
}

// TestPublishDropsVanishedSeries — снятая с манифеста нода обязана исчезнуть из
// выдачи, а не застыть на последнем значении. Иначе метрика утверждала бы, что
// ноду опрашивали семь секунд назад, спустя месяц после её удаления.
func TestPublishDropsVanishedSeries(t *testing.T) {
	registry := New()
	registry.publish(sampleStats(), sampleTime)

	if got := seriesCount(t, registry, "spiritvpn_node_usage_pull_age_seconds"); got != 1 {
		t.Fatalf("серий возраста опроса %d, ожидалась 1: снимок не применился", got)
	}

	vanished := sampleStats()
	vanished.Cursors = nil
	registry.publish(vanished, sampleTime)

	if got := seriesCount(t, registry, "spiritvpn_node_usage_pull_age_seconds"); got != 0 {
		t.Errorf("серий возраста опроса %d, ожидалось 0: серия ушедшей ноды осталась", got)
	}
}

// TestPublishKeepsEnumZeros — сброс векторов стирает и предзаполнение нулями,
// поэтому publish обязан его восстановить. Без этого статус, у которого сейчас
// нет строк, исчезал бы после первого же снимка.
func TestPublishKeepsEnumZeros(t *testing.T) {
	registry := New()
	registry.publish(sampleStats(), sampleTime)

	if got := seriesCount(t, registry, "spiritvpn_agent_operations"); got != len(operationStatuses) {
		t.Errorf("серий статусов %d, ожидалось %d", got, len(operationStatuses))
	}
	if got := testutil.ToFloat64(registry.operations.WithLabelValues("FAILED_PERMANENT")); got != 0 {
		t.Errorf("статус без строк показывает %v, ожидался 0", got)
	}
}

func TestStatsWorkerPublishesSnapshot(t *testing.T) {
	registry := New()
	worker := registry.StatsWorker(&fakeStatsRepo{stats: sampleStats()})

	progressed, err := worker.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("шаг снимка: %v", err)
	}

	// progressed=false — это не «ничего не произошло», а темп: цикл runWorker
	// спит idle-интервал именно на пустом проходе, и снимку нужен ровно он.
	if progressed {
		t.Error("шаг сообщил о прогрессе, из-за чего цикл крутился бы без пауз")
	}
	if got := testutil.ToFloat64(registry.manifestRevision); got != 9 {
		t.Errorf("ревизия манифеста %v, ожидалась 9", got)
	}
	if got := testutil.ToFloat64(registry.statsRefreshes.WithLabelValues(resultOK)); got != 1 {
		t.Errorf("успешных снимков %v, ожидался 1", got)
	}
}

// TestStatsWorkerFailureIsVisible — без этого отказ воркера выглядит как
// застывшие, но правдоподобные значения всех gauge из БД.
func TestStatsWorkerFailureIsVisible(t *testing.T) {
	registry := New()
	worker := registry.StatsWorker(&fakeStatsRepo{err: errors.New("база недоступна")})

	if _, err := worker.ProcessNext(context.Background()); err == nil {
		t.Fatal("отказ снимка не вернул ошибку, значит цикл не сделает backoff и не залогирует")
	}

	if got := testutil.ToFloat64(registry.statsRefreshes.WithLabelValues(resultError)); got != 1 {
		t.Errorf("отказов %v, ожидался 1", got)
	}
	if got := testutil.ToFloat64(registry.statsRefreshedAt); got != 0 {
		t.Errorf("момент снимка %v, ожидался 0: отказ не должен выглядеть свежим снимком", got)
	}
}
