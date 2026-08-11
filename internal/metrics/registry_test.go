package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// seriesCount — сколько серий у семейства метрик.
//
// Ноль означает и «серий нет», и «семейства нет»: Prometheus не выдаёт вектор без
// единой серии, и различить эти два случая снаружи нельзя. Для проверок ниже это
// одно и то же — метрики с таким набором меток в выдаче не будет.
func seriesCount(t *testing.T, r *Registry, name string) int {
	t.Helper()

	families, err := r.reg.Gather()
	if err != nil {
		t.Fatalf("сбор метрик: %v", err)
	}

	for _, f := range families {
		if f.GetName() == name {
			return len(f.GetMetric())
		}
	}
	return 0
}

// TestRegistryPassesPrometheusLint — имена метрик являются контрактом
// наблюдаемости: на них вешаются alert'ы, и переименование после выкатки ломает
// их молча. promlint ловит нарушения конвенций (единицы измерения в имени,
// _total у счётчиков, отсутствие HELP) до того, как имя куда-то попало.
//
// Проверяются только собственные метрики: go_* и process_* приходят из
// client_golang, и править их всё равно нельзя.
func TestRegistryPassesPrometheusLint(t *testing.T) {
	problems, err := testutil.GatherAndLint(New().reg)
	if err != nil {
		t.Fatalf("линт метрик: %v", err)
	}

	for _, problem := range problems {
		if strings.HasPrefix(problem.Metric, namespace+"_") {
			t.Errorf("%s: %s", problem.Metric, problem.Text)
		}
	}
}

// TestNoCustomerIDLabels — §15 дословно: customer ID допускается только в
// ограниченных audit records и НЕ используется как metric label.
//
// Проверка идёт по белому списку, а не поиском строки "customer_id": так она
// ловит и метку, названную иначе (client, subscriber, account), потому что любая
// новая метка обязана быть внесена сюда осознанно.
func TestNoCustomerIDLabels(t *testing.T) {
	allowed := map[string]bool{
		labelNode: true, labelMethod: true, labelCode: true, labelStatus: true,
		labelDesiredState: true, labelApplyState: true, labelReason: true,
		labelWorker: true, labelResult: true,
		// Метка коллектора пула; объявлена строкой, а не константой.
		"state": true,
	}

	registry := New()
	// Снимок наполняется реальными значениями: пустой реестр не содержит серий с
	// динамическими метками, и проверка на нём была бы вакуумной.
	registry.publish(sampleStats(), sampleTime)
	observeSampleAgentCall(registry)

	families, err := registry.reg.Gather()
	if err != nil {
		t.Fatalf("сбор метрик: %v", err)
	}

	for _, f := range families {
		if !strings.HasPrefix(f.GetName(), namespace+"_") {
			continue
		}
		for _, metric := range f.GetMetric() {
			for _, label := range metric.GetLabel() {
				if !allowed[label.GetName()] {
					t.Errorf("метрика %s несёт метку %q, которой нет в белом списке §15",
						f.GetName(), label.GetName())
				}
			}
		}
	}
}

// TestEnumSeriesStartAtZero — статус без строк обязан показывать ноль, а не
// исчезать. Пропавшая серия ломает alert тише всего: сравнивать не с чем, и
// правило просто никогда не срабатывает.
func TestEnumSeriesStartAtZero(t *testing.T) {
	registry := New()

	if got := seriesCount(t, registry, "spiritvpn_agent_operations"); got != len(operationStatuses) {
		t.Errorf("серий статусов операций %d, ожидалось %d", got, len(operationStatuses))
	}

	want := len(desiredStates) * len(applyStates)
	if got := seriesCount(t, registry, "spiritvpn_accesses"); got != want {
		t.Errorf("серий access %d, ожидалось %d", got, want)
	}
}
