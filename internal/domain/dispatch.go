package domain

import "time"

// OperationStatus — статус строки agent_operations (§9).
//
// Жизненный цикл:
//
//	PENDING → IN_FLIGHT → SUCCEEDED
//	   |          ├→ RETRY_WAIT → IN_FLIGHT
//	   |          ├→ FAILED_PERMANENT
//	   |          └→ SUPERSEDED (устаревшая версия после истечения lease)
//	   └→ SUPERSEDED
type OperationStatus string

const (
	OperationStatusPending         OperationStatus = "PENDING"
	OperationStatusInFlight        OperationStatus = "IN_FLIGHT"
	OperationStatusSucceeded       OperationStatus = "SUCCEEDED"
	OperationStatusRetryWait       OperationStatus = "RETRY_WAIT"
	OperationStatusFailedPermanent OperationStatus = "FAILED_PERMANENT"
	OperationStatusSuperseded      OperationStatus = "SUPERSEDED"
)

// AttemptOutcome — классифицированный исход одной попытки доставки.
//
// Транспорт сюда не просачивается намеренно: агент отвечает и gRPC-кодом, и
// ApplyStatus в теле, но домену важно только, чем это кончилось для операции.
// Сведение двух каналов в один исход — работа адаптера nodeagent.
type AttemptOutcome string

const (
	AttemptSucceeded AttemptOutcome = "SUCCEEDED"
	AttemptRetryable AttemptOutcome = "RETRYABLE"
	AttemptPermanent AttemptOutcome = "PERMANENT"
)

// Параметры backoff из §9: начальная задержка 1 секунда, максимум 5 минут,
// число попыток не ограничено, пока desired state актуален.
const (
	BackoffInitial = time.Second
	BackoffMax     = 5 * time.Minute
)

// OperationResultPlan — что записать по итогу одной попытки (§9).
type OperationResultPlan struct {
	Status OperationStatus
	// NextAttemptAt непусто только у RETRY_WAIT: остальные статусы либо
	// терминальны, либо ждут не времени, а нового desired state.
	NextAttemptAt *time.Time
	// ApplyState — как исход проецируется на строку access (решение 41).
	ApplyState ApplyState
	// Completed отмечает терминальный исход: он же completed_at строки операции.
	Completed bool
}

// PlanOperationResult раскладывает исход попытки в состояние операции и access
// (§9, решение 41).
//
// jitter приходит параметром в [0,1): домен детерминирован и случайности не
// производит, как и времени. Вызывающий слой берёт его у своего источника.
func PlanOperationResult(
	outcome AttemptOutcome,
	attemptCount int32,
	now time.Time,
	jitter float64,
) OperationResultPlan {
	switch outcome {
	case AttemptSucceeded:
		return OperationResultPlan{
			Status:     OperationStatusSucceeded,
			ApplyState: ApplyStateApplied,
			Completed:  true,
		}

	case AttemptPermanent:
		// Терминальное состояние. §9: восстановление выполняется изменением
		// некорректного desired state либо повторным ReconcileUsers, а не
		// бесконечным повтором — permanent-ошибки не хот-лупятся.
		return OperationResultPlan{
			Status:     OperationStatusFailedPermanent,
			ApplyState: ApplyStateFailed,
			Completed:  true,
		}

	default:
		next := now.Add(BackoffDelay(attemptCount, jitter))
		return OperationResultPlan{
			Status:        OperationStatusRetryWait,
			NextAttemptAt: &next,
			ApplyState:    ApplyStateRetrying,
		}
	}
}

// BackoffDelay — задержка перед следующей попыткой (§9).
//
// Экспонента от BackoffInitial с потолком BackoffMax. Jitter применяется к
// половине задержки, а не ко всей: полный jitter мог бы дать задержку около
// нуля, и после отказа недоступной ноды сотня операций ушла бы на неё сразу же.
// Нижняя половина фиксирована и гарантирует паузу, верхняя разводит попытки во
// времени, чтобы они не били в агента синхронным залпом.
func BackoffDelay(attemptCount int32, jitter float64) time.Duration {
	if attemptCount < 0 {
		attemptCount = 0
	}

	delay := BackoffInitial
	for range attemptCount {
		delay *= 2
		if delay >= BackoffMax {
			delay = BackoffMax
			break
		}
	}

	half := delay / 2
	return half + time.Duration(float64(half)*clampUnit(jitter))
}

// clampUnit приводит jitter к [0,1): испорченный источник случайности не должен
// превращаться в отрицательную или гигантскую задержку.
func clampUnit(v float64) float64 {
	switch {
	case !(v >= 0):
		// Ложь и для отрицательных, и для NaN.
		return 0
	case v >= 1:
		return 1
	default:
		return v
	}
}
