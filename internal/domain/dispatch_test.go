package domain

import (
	"math"
	"testing"
	"time"
)

func TestPlanOperationResult(t *testing.T) {
	tests := []struct {
		name       string
		outcome    AttemptOutcome
		wantStatus OperationStatus
		wantApply  ApplyState
		wantRetry  bool
		completed  bool
	}{
		{
			name:       "успех",
			outcome:    AttemptSucceeded,
			wantStatus: OperationStatusSucceeded,
			wantApply:  ApplyStateApplied,
			completed:  true,
		},
		{
			// Permanent-ошибки не хот-лупятся, восстановление — через
			// изменение desired state или ReconcileUsers.
			name:       "постоянный отказ терминален",
			outcome:    AttemptPermanent,
			wantStatus: OperationStatusFailedPermanent,
			wantApply:  ApplyStateFailed,
			completed:  true,
		},
		{
			name:       "временный отказ ждёт следующей попытки",
			outcome:    AttemptRetryable,
			wantStatus: OperationStatusRetryWait,
			wantApply:  ApplyStateRetrying,
			wantRetry:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := PlanOperationResult(tt.outcome, 0, tNow, 0.5)

			if plan.Status != tt.wantStatus {
				t.Errorf("статус %s, ожидался %s", plan.Status, tt.wantStatus)
			}
			if plan.ApplyState != tt.wantApply {
				t.Errorf("apply_state %s, ожидался %s", plan.ApplyState, tt.wantApply)
			}
			if plan.Completed != tt.completed {
				t.Errorf("completed %v, ожидалось %v", plan.Completed, tt.completed)
			}

			switch {
			case tt.wantRetry && plan.NextAttemptAt == nil:
				t.Error("нет времени следующей попытки: операция не будет подхвачена")
			case !tt.wantRetry && plan.NextAttemptAt != nil:
				t.Errorf("терминальный исход несёт next_attempt_at %v", plan.NextAttemptAt)
			}
		})
	}
}

// TestSupersededStopsRetriesOnly — устаревшая desired_version
// прекращает повторы, но не переписывает уже состоявшийся терминальный исход.
func TestSupersededStopsRetriesOnly(t *testing.T) {
	retry := PlanOperationResult(AttemptRetryable, 0, tNow, 0.5).Superseded()

	if retry.Status != OperationStatusSuperseded {
		t.Errorf("статус %s, ожидался %s", retry.Status, OperationStatusSuperseded)
	}
	if retry.NextAttemptAt != nil {
		t.Errorf("устаревшая операция запланирована на %v: её никто не должен повторять", retry.NextAttemptAt)
	}
	if !retry.Completed {
		t.Error("SUPERSEDED терминален и обязан проставлять completed_at")
	}

	// Журнал исполнения остаётся правдивым: операция действительно доехала, и
	// смена desired state задним числом этого не отменяет.
	for _, outcome := range []AttemptOutcome{AttemptSucceeded, AttemptPermanent} {
		plan := PlanOperationResult(outcome, 0, tNow, 0.5)
		if got := plan.Superseded(); got != plan {
			t.Errorf("исход %s переписан: %+v", outcome, got)
		}
	}
}

// TestBackoffDelayGrowsAndCaps — от 1 секунды экспоненциально до потолка в
// 5 минут.
func TestBackoffDelayGrowsAndCaps(t *testing.T) {
	// При нулевом jitter задержка равна нижней половине — она фиксирована.
	previous := time.Duration(0)
	for attempt := range int32(12) {
		got := BackoffDelay(attempt, 0)

		if got < previous {
			t.Fatalf("попытка %d: задержка %s меньше предыдущей %s", attempt, got, previous)
		}
		if got > BackoffMax {
			t.Fatalf("попытка %d: задержка %s превышает потолок %s", attempt, got, BackoffMax)
		}
		previous = got
	}

	if got := BackoffDelay(0, 0); got != BackoffInitial/2 {
		t.Errorf("первая задержка %s, ожидалась половина от %s", got, BackoffInitial)
	}
	// Потолок достигается и дальше не растёт.
	if BackoffDelay(100, 1) != BackoffMax {
		t.Errorf("задержка на сотой попытке %s, ожидался потолок", BackoffDelay(100, 1))
	}
}

// TestBackoffDelayKeepsFloor — полный jitter мог бы дать задержку около нуля, и
// после отказа недоступной ноды сотня операций ушла бы на неё немедленно.
// Половина задержки фиксирована именно ради этого.
func TestBackoffDelayKeepsFloor(t *testing.T) {
	for _, jitter := range []float64{0, 0.5, 1} {
		if got := BackoffDelay(3, jitter); got < BackoffInitial*4 {
			t.Errorf("jitter %v: задержка %s меньше гарантированного минимума", jitter, got)
		}
	}
}

// TestBackoffDelayIgnoresBrokenJitter — испорченный источник случайности не
// должен превращаться в отрицательную или гигантскую паузу.
func TestBackoffDelayIgnoresBrokenJitter(t *testing.T) {
	base := BackoffDelay(2, 0)
	full := BackoffDelay(2, 1)

	for _, jitter := range []float64{-1, math.NaN(), math.Inf(1), 42} {
		got := BackoffDelay(2, jitter)
		if got < base || got > full {
			t.Errorf("jitter %v дал задержку %s вне [%s, %s]", jitter, got, base, full)
		}
	}
}

// TestBackoffDelayHandlesNegativeAttempt — счётчик попыток приходит из БД, и
// повреждённое значение не должно ронять расчёт.
func TestBackoffDelayHandlesNegativeAttempt(t *testing.T) {
	if got := BackoffDelay(-5, 0); got != BackoffDelay(0, 0) {
		t.Fatalf("отрицательный счётчик дал %s", got)
	}
}
