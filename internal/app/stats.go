package app

import "context"

// StatsRepository — снимок состояния БД для метрик §15.
//
// Порт read-only и вне транзакции: снимок ничего не решает и ни с чем не
// сверяется, а согласованность между его частями никому не нужна — каждое
// значение уезжает в свой независимый gauge. Транзакция здесь означала бы
// удерживать снапшот ради консистентности чисел, которые всё равно устареют
// раньше следующего scrape.
//
// Use case у него нет намеренно: доменных решений тут ноль, и обёртка над одним
// вызовом была бы пустым слоем. Потребитель — internal/metrics.
type StatsRepository interface {
	CollectStats(ctx context.Context) (Stats, error)
}

// Stats — то, что §15 требует показать, но что живёт в БД, а не в процессе.
//
// Вторая половина метрик (latency вызовов агента, ошибки расшифрования, health
// ноды) наблюдается по ходу работы и сюда не попадает: её снимают декораторы
// портов в internal/metrics.
type Stats struct {
	// Operations — agent operations по status: количество и возраст самой старой.
	Operations []OperationStat
	// Accesses — количество access по паре (desired_state, apply_state).
	Accesses []AccessStat
	// Cursors — состояние опроса по каждой текущей ноде.
	Cursors []NodeCursorStat
	// Quarantine — карантинные usage-items по причине.
	Quarantine []QuarantineStat
	// Leases — занятость lease по воркерам. Expiry сюда не входит: он обходится
	// row lock'ом вместо lease (решение 54), и показывать у него нечего.
	Leases []LeaseStat

	// ManifestRevision — последняя принятая ревизия манифеста (§6).
	ManifestRevision int64
	// MaterializedRevision — последняя ревизия, чей fan-out завершён (§13).
	// Отставание от ManifestRevision и есть materialization lag в ревизиях.
	MaterializedRevision int64
	// MaterializationLagSeconds — возраст самой старой незавершённой джобы.
	// Ноль означает, что незавершённых нет, а не что лаг нулевой.
	MaterializationLagSeconds float64

	// ExpiredCustomers — customer с наступившим expires_at, у которых ещё остался
	// PRESENT-access. Это не «сколько истекло», а «сколько истекло и не погашено»:
	// у погашенных снимать уже нечего, и в очереди воркера их нет.
	ExpiredCustomers int64
	// ExpiryLagSeconds — насколько просрочен самый старый непогашенный customer.
	// Прямая мера задержки expiry worker (§15).
	ExpiryLagSeconds float64

	// ExhaustedNodeQuotas — ноды с исчерпанной квотой в ОТКРЫТЫХ периодах.
	// Закрытые периоды считать бессмысленно: их exhausted_at — история, а не
	// действующая блокировка (§12).
	ExhaustedNodeQuotas int64

	// SchemaVersion и SchemaDirty — совместимость миграций (§15). Сверять с
	// версией, зашитой в бинарь, — работа потребителя: она константа процесса и
	// в БД её нет.
	SchemaVersion int64
	SchemaDirty   bool
}

// OperationStat — agent operations одного статуса.
type OperationStat struct {
	Status string
	Count  int64
	// OldestAgeSeconds — возраст самой старой операции статуса. Для терминальных
	// статусов бессмысленен (строки не удаляются), для PENDING и RETRY_WAIT —
	// главный сигнал застревания очереди.
	OldestAgeSeconds float64
}

// AccessStat — количество access в одном сочетании состояний.
type AccessStat struct {
	DesiredState string
	ApplyState   string
	Count        int64
}

// NodeCursorStat — состояние опроса одной ноды (§12).
type NodeCursorStat struct {
	NodeID string
	// LastPullAgeSeconds — время с последней ПОПЫТКИ опроса, а не с последнего
	// сдвига курсора (решение 61). Растёт, когда воркер до ноды не доходит.
	LastPullAgeSeconds float64
	// AckedSequence — подтверждённая позиция в спуле. Само по себе число ни о чём
	// не говорит, но его остановка при живом опросе означает, что нода перестала
	// отдавать трафик.
	AckedSequence uint64
	// LeaseExpired — lease есть, но просрочен: воркер умер, не сняв его.
	// Занятый lease отдельной серией не показывается: это штатная работа, а
	// суммарную занятость даёт Leases.
	LeaseExpired bool
}

// QuarantineStat — карантинные items одной причины (§12).
type QuarantineStat struct {
	Reason string
	Count  int64
}

// LeaseStat — занятость lease одного воркера.
//
// Worker совпадает с именем в логах runWorker, чтобы метрика и запись об отказе
// сходились по одному значению.
type LeaseStat struct {
	Worker  string
	Held    int64
	Expired int64
}
