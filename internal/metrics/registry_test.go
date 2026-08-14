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

// TestNoCustomerIDLabels — customer ID допускается только в ограниченных audit
// records и не используется как metric label.
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
	// динамическими метками, и проверка на нём проходила бы при любом коде.
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
					t.Errorf("метрика %s несёт метку %q, которой нет в белом списке",
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

// Версия схемы, встроенная в бинарь, приезжает не из снимка базы: в базе её нет.
// Публикует её composition root один раз при старте, и до этого вызова серия
// показывает ноль. Ноль здесь неотличим от «миграций нет», поэтому проверяется
// именно переход к заданному значению.
//
// Пара с schema_version — то, по чему оператор отличает штатный rollout от
// застрявшего процесса: обе метрики читаются рядом, и та же разница решает,
// ответит ли /health/ready.
func TestBinarySchemaVersionIsPublished(t *testing.T) {
	registry := New()

	const name = "spiritvpn_binary_schema_version"
	if got := testutil.ToFloat64(registry.binarySchemaVersion); got != 0 {
		t.Fatalf("до публикации %s = %v, ожидался ноль", name, got)
	}

	registry.SetBinarySchemaVersion(7)

	if got := testutil.ToFloat64(registry.binarySchemaVersion); got != 7 {
		t.Errorf("%s = %v, ожидалось 7", name, got)
	}
	// Серия обязана быть в выдаче: gauge без Set в /metrics всё равно попадает,
	// но проверка через реестр ловит и то, что метрика в нём зарегистрирована.
	if got := seriesCount(t, registry, name); got != 1 {
		t.Errorf("серий %s %d, ожидалась 1: метрика не зарегистрирована в реестре", name, got)
	}
}
