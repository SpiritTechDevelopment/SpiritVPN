// Package metrics — реестр метрик Prometheus и инструментирование адаптеров (§15).
//
// Устроен так, чтобы ни domain, ни app про Prometheus не знали. Метрики делятся
// на две половины по источнику, и у каждой свой механизм:
//
//	наблюдаемое по ходу работы — декораторы портов app (agent.go, sealer.go).
//	Composition root оборачивает ими уже существующие адаптеры, поэтому ни один
//	use case не меняется и опциональных зависимостей ни у кого не появляется.
//
//	состояние БД — снимок, который снимает воркер (stats.go) на своём интервале.
//	Не на scrape: иначе стоимость наблюдения задавал бы Prometheus снаружи
//	процесса, а /metrics зависал бы вместе с базой.
//
// Метки customer_id нет ни у одной метрики и быть не может: §15 разрешает customer
// ID только в audit records и прямо запрещает как metric label. Это проверяется
// тестом, а не соблюдается на глаз.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// namespace — префикс всех метрик процесса.
const namespace = "spiritvpn"

// Имена меток. Константами, потому что каждое встречается и при объявлении
// коллектора, и при записи значения, и разъехавшаяся пара даёт панику только в
// рантайме.
const (
	labelNode         = "node"
	labelMethod       = "method"
	labelCode         = "code"
	labelStatus       = "status"
	labelDesiredState = "desired_state"
	labelApplyState   = "apply_state"
	labelReason       = "reason"
	labelWorker       = "worker"
	labelResult       = "result"
	labelKind         = "kind"
)

// Виды изменений, которые находит reconcile. Метка kind у reconcile_drift_total.
//
// unchanged серией не заводится: она означает отсутствие дрейфа, то есть штатную
// работу, и на каждом проходе давала бы счётчик размером со всю ноду, в котором
// интересное тонет.
const (
	driftAdded    = "added"
	driftReplaced = "replaced"
	driftRemoved  = "removed"
)

// Пригодность снимка Xray. Метка result у inventory_observations_total.
const (
	resultComplete    = "complete"
	resultIncomplete  = "incomplete"
	resultNotObserved = "not_observed"
)

// Имена RPC агента для метки method. Совпадают с именами методов в
// NodeAgentService, чтобы метрика и контракт агента читались как одно.
const (
	methodEnsureUserPresent = "EnsureUserPresent"
	methodEnsureUserAbsent  = "EnsureUserAbsent"
	methodGetNodeState      = "GetNodeState"
	methodReconcileUsers    = "ReconcileUsers"
	// methodObserveUsers — тот же GetNodeState, но с include_users. Отдельная
	// метка, потому что это другой по смыслу и по цене вызов: инвентарь ноды
	// заметно крупнее пачки дельт, и мешать их латентности в одну серию значило
	// бы не видеть ни одной.
	methodObserveUsers = "ObserveUsers"
)

// Полные наборы значений enum-колонок, которыми заполняются серии со счётчиком
// ноль.
//
// Дублируют CHECK-ограничения 0001_baseline.up.sql намеренно: без предзаполнения
// статус, у которого сейчас нет строк, ИСЧЕЗАЕТ из выдачи вместо того, чтобы
// показать ноль. Пропавшая серия — классическая причина alert'а, который никогда
// не срабатывает, потому что сравнивать не с чем.
var (
	operationStatuses = []string{
		"PENDING", "IN_FLIGHT", "SUCCEEDED", "RETRY_WAIT", "FAILED_PERMANENT", "SUPERSEDED",
	}
	desiredStates = []string{"PRESENT", "ABSENT"}
	applyStates   = []string{"PENDING", "APPLIED", "RETRYING", "FAILED"}
)

// callBuckets покрывают оба deadline агента: DefaultCallTimeout (5 секунд) и
// DefaultPullTimeout (30). Стандартные корзины client_golang заканчиваются на 10
// и сложили бы весь долгий pull в +Inf.
var callBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// Registry владеет метриками процесса и реестром, из которого их отдаёт /metrics.
type Registry struct {
	reg *prometheus.Registry

	// Наблюдается по ходу работы: декораторы портов.
	agentCallDuration    *prometheus.HistogramVec
	agentCalls           *prometheus.CounterVec
	agentLastSuccess     *prometheus.GaugeVec
	agentAlerts          *prometheus.CounterVec
	usagePullCapped      *prometheus.CounterVec
	nodeXrayReachable    *prometheus.GaugeVec
	nodeXrayUptime       *prometheus.GaugeVec
	nodeNeedsBootstrap   *prometheus.GaugeVec
	credentialOpenErrors prometheus.Counter
	usageDedupPruned     prometheus.Counter
	reconcileDrift       *prometheus.CounterVec
	inventoryObserved    *prometheus.CounterVec

	// Снимок БД.
	operations           *prometheus.GaugeVec
	operationOldestAge   *prometheus.GaugeVec
	accesses             *prometheus.GaugeVec
	cursorAge            *prometheus.GaugeVec
	cursorAcked          *prometheus.GaugeVec
	cursorLeaseExpired   *prometheus.GaugeVec
	quarantine           *prometheus.GaugeVec
	leasesHeld           *prometheus.GaugeVec
	leasesExpired        *prometheus.GaugeVec
	manifestRevision     prometheus.Gauge
	materializedRevision prometheus.Gauge
	materializationLag   prometheus.Gauge
	expiredCustomers     prometheus.Gauge
	expiryLag            prometheus.Gauge
	exhaustedQuotas      prometheus.Gauge
	usageDedupOldestAge  prometheus.Gauge
	schemaVersion        prometheus.Gauge
	schemaDirty          prometheus.Gauge
	binarySchemaVersion  prometheus.Gauge
	statsRefreshes       *prometheus.CounterVec
	statsRefreshedAt     prometheus.Gauge
}

// New собирает реестр со всеми метриками процесса.
//
// Реестр собственный, а не prometheus.DefaultRegisterer: глобальный принимает
// регистрации из любой транзитивной зависимости, и состав /metrics переставал бы
// быть свойством этого кода. Побочная выгода — тесты не делят состояние.
func New() *Registry {
	r := &Registry{reg: prometheus.NewRegistry()}

	r.agentCallDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "agent_call_duration_seconds",
		Help:      "Duration of RPC calls to node-agent, by method and outcome code.",
		Buckets:   callBuckets,
	}, []string{labelMethod, labelCode})

	// Метка node есть у счётчика, но не у гистограммы: у гистограммы серий в
	// число корзин больше, и добавление ноды третьим измерением умножило бы их на
	// размер fleet. §15 требует latency и «последний успешный вызов по node»;
	// измерение по нодам отдано счётчику и метке последнего успеха.
	r.agentCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "agent_calls_total",
		Help:      "RPC calls to node-agent, by node, method and outcome code.",
	}, []string{labelNode, labelMethod, labelCode})

	r.agentLastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "agent_last_success_timestamp_seconds",
		Help:      "Unix timestamp of the last successful RPC to node-agent.",
	}, []string{labelNode})

	// Отдельный счётчик, а не фильтр по коду: Outcome.Alert выставляют исходы с
	// РАЗНЫМИ кодами (подмена идентичности, неопознанный transport, непригодный
	// agent_config), и правило alert'а не должно перечислять их руками. Он же
	// закрывает решение 52, где alert был временно подменён логом уровня ERROR.
	r.agentAlerts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "agent_alert_outcomes_total",
		Help:      "RPC outcomes to node-agent that require operator attention (spec §9).",
	}, []string{labelNode, labelCode})

	r.usagePullCapped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "usage_pull_capped_total",
		Help:      "Polls where the agent hit the batch cap: more usage is left in its spool.",
	}, []string{labelNode})

	// Три следующих gauge — ПОСЛЕДНЕЕ НАБЛЮДЁННОЕ состояние ноды, а не текущее.
	// Снятая с манифеста нода перестаёт опрашиваться, и её серии застывают на
	// последнем значении: перечислить свои метки GaugeVec не умеет, а вести
	// параллельный реестр нод значило бы взять блокировку на каждом RPC ради
	// события, которое случается при правке манифеста.
	//
	// Признак живости здесь — agent_last_success_timestamp_seconds, и читать эти
	// три без него нельзя. Авторитетный состав fleet даёт снимок БД: его
	// node_usage_pull_age_seconds сбрасывается каждый раз и снятую ноду теряет.
	r.nodeXrayReachable = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "node_xray_reachable",
		Help:      "Xray was reachable by the agent as of the last poll: 1 or 0.",
	}, []string{labelNode})

	r.nodeXrayUptime = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "node_xray_uptime_seconds",
		Help:      "Xray uptime on the node as of the last poll.",
	}, []string{labelNode})

	r.nodeNeedsBootstrap = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "node_needs_bootstrap",
		Help:      "Agent needs authoritative reconcile before it may remove users (spec §10): 1 or 0.",
	}, []string{labelNode})

	// Без меток: и место расшифровки (диспетчер или выдача ссылок), и причина —
	// инфраструктурные, а нода тут ни при чём. Любое ненулевое значение означает
	// расхождение ключа и требует вмешательства (§15).
	r.credentialOpenErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "credential_open_errors_total",
		Help:      "Failures to decrypt client_uuid.",
	})

	r.usageDedupPruned = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "usage_dedup_rows_pruned_total",
		Help:      "Usage dedup rows deleted by retention.",
	})

	// Каждое изменение, найденное reconcile, — это дрейф: обычные изменения
	// доставляет dispatcher, и к приходу полного набора нода уже должна совпадать
	// с desired. Устойчиво ненулевой rate означает, что что-то расходится молча.
	r.reconcileDrift = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "reconcile_drift_total",
		Help:      "User changes an authoritative reconcile had to make, by node and kind.",
	}, []string{labelNode, labelKind})

	// Пригодность инвентаря считается отдельно от успеха вызова: агент, который
	// исправно отвечает усечённым снимком, из сверки выпадает молча. Такая нода
	// не показывает расхождений просто потому, что сравнивать не с чем, и без
	// этой серии выглядела бы благополучнее исправной (§10).
	r.inventoryObserved = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "inventory_observations_total",
		Help:      "Xray inventory observations, by node and usability of the snapshot.",
	}, []string{labelNode, labelResult})

	r.operations = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "agent_operations",
		Help:      "Agent operations, by status.",
	}, []string{labelStatus})

	r.operationOldestAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "agent_operation_oldest_age_seconds",
		Help:      "Age of the oldest agent operation in each status.",
	}, []string{labelStatus})

	r.accesses = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "accesses",
		Help:      "Non-retired accesses, by desired and apply state.",
	}, []string{labelDesiredState, labelApplyState})

	r.cursorAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "node_usage_pull_age_seconds",
		Help:      "Time since the last usage poll attempt for this node.",
	}, []string{labelNode})

	r.cursorAcked = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "node_usage_acked_sequence",
		Help:      "Acknowledged position in the node's usage spool.",
	}, []string{labelNode})

	r.cursorLeaseExpired = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "node_usage_lease_expired",
		Help:      "The node holds an expired usage-poll lease: 1 or 0.",
	}, []string{labelNode})

	r.quarantine = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "traffic_quarantine_rows",
		Help:      "Quarantined usage items, by reason. Cumulative until retention runs.",
	}, []string{labelReason})

	r.leasesHeld = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "worker_leases_held",
		Help:      "Currently held leases, by worker.",
	}, []string{labelWorker})

	r.leasesExpired = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "worker_leases_expired",
		Help:      "Expired leases by worker: the holder died without releasing them.",
	}, []string{labelWorker})

	r.manifestRevision = newGauge("manifest_revision",
		"Latest accepted manifest revision.")
	r.materializedRevision = newGauge("manifest_materialized_revision",
		"Latest manifest revision whose fan-out has completed.")
	r.materializationLag = newGauge("manifest_materialization_lag_seconds",
		"Age of the oldest unfinished materialization job; 0 means there are none.")
	r.expiredCustomers = newGauge("expired_customers",
		"Customers past their expiry that still hold a PRESENT access.")
	r.expiryLag = newGauge("expiry_lag_seconds",
		"How far past expiry the oldest not-yet-revoked customer is.")
	r.exhaustedQuotas = newGauge("exhausted_node_quotas",
		"Nodes whose quota is exhausted within an open period.")
	r.usageDedupOldestAge = newGauge("usage_dedup_oldest_age_seconds",
		"Age of the oldest usage dedup row. Sits near the retention window; alert above it.")
	r.schemaVersion = newGauge("schema_version",
		"Schema version present in the database.")
	r.schemaDirty = newGauge("schema_dirty",
		"Schema is marked dirty, i.e. a migration was interrupted: 1 or 0.")
	r.binarySchemaVersion = newGauge("binary_schema_version",
		"Schema version built into this binary. Lower than schema_version is a normal rollout.")
	r.statsRefreshedAt = newGauge("stats_last_refresh_timestamp_seconds",
		"Unix timestamp of the last successful database state snapshot.")

	// Самонаблюдение снимка обязательно: без него отказ воркера выглядит как
	// застывшие, но правдоподобные значения всех gauge из БД.
	r.statsRefreshes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "stats_refreshes_total",
		Help:      "Attempts to snapshot database state, by outcome.",
	}, []string{labelResult})

	r.reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),

		r.agentCallDuration, r.agentCalls, r.agentLastSuccess, r.agentAlerts,
		r.usagePullCapped, r.nodeXrayReachable, r.nodeXrayUptime, r.nodeNeedsBootstrap,
		r.credentialOpenErrors, r.usageDedupPruned, r.reconcileDrift, r.inventoryObserved,

		r.operations, r.operationOldestAge, r.accesses,
		r.cursorAge, r.cursorAcked, r.cursorLeaseExpired, r.quarantine,
		r.leasesHeld, r.leasesExpired,
		r.manifestRevision, r.materializedRevision, r.materializationLag,
		r.expiredCustomers, r.expiryLag, r.exhaustedQuotas, r.usageDedupOldestAge,
		r.schemaVersion, r.schemaDirty, r.binarySchemaVersion,
		r.statsRefreshes, r.statsRefreshedAt,
	)

	r.seedEnums()

	// Счётчик снимков заводится с нулём по обоим исходам: rate() по серии,
	// появляющейся только после первого отказа, до этого момента не существует —
	// то есть alert на отказы снимка молчал бы именно до первого отказа.
	r.statsRefreshes.WithLabelValues(resultOK).Add(0)
	r.statsRefreshes.WithLabelValues(resultError).Add(0)

	return r
}

// newGauge — сокращение для скаляра без меток.
func newGauge(name, help string) prometheus.Gauge {
	return prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      name,
		Help:      help,
	})
}

// seedEnums заводит серии с нулём для всех значений enum-колонок.
//
// Вызывается один раз на старте и повторно после каждого Reset снимка: сброс
// стирает и предзаполнение тоже.
func (r *Registry) seedEnums() {
	for _, status := range operationStatuses {
		r.operations.WithLabelValues(status).Set(0)
	}
	for _, desired := range desiredStates {
		for _, apply := range applyStates {
			r.accesses.WithLabelValues(desired, apply).Set(0)
		}
	}
}

// Handler отдаёт /metrics.
//
// ContinueOnError, а не отказ всего ответа: один сломавшийся коллектор не должен
// лишать оператора остальных метрик — наблюдаемость нужнее всего именно тогда,
// когда что-то сломано.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError})
}

// SetBinarySchemaVersion фиксирует версию миграций, встроенных в бинарь (§15).
//
// Константа процесса, в базе её нет, поэтому со снимком она не приезжает.
func (r *Registry) SetBinarySchemaVersion(version uint) {
	r.binarySchemaVersion.Set(float64(version))
}

// boolGauge переводит флаг в значение gauge: у Prometheus нет булева типа, и
// договор «1 или 0» записан в Help каждой такой метрики.
func boolGauge(v bool) float64 {
	if v {
		return 1
	}
	return 0
}
