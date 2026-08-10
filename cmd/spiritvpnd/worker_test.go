package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// Тесты цикла проверяют ровно одно: что воркер останавливается по отмене в любой
// его точке. Всё содержательное — в use case; здесь важна только реакция на
// сигнал, потому что зависший цикл делает под неубиваемым до конца grace period.

// scriptedWorker отдаёт заранее заданную последовательность исходов, а затем
// отменяет контекст, чтобы цикл завершился.
type scriptedWorker struct {
	mu      sync.Mutex
	results []workerStep
	// calls — сколько запланированных шагов израсходовано; invocations — сколько
	// раз цикл вообще вызвал воркер. Их расхождение и показывает, продолжил ли
	// цикл работу после отказа.
	calls       int
	invocations int
	cancel      context.CancelFunc
}

type workerStep struct {
	progressed bool
	err        error
}

func (w *scriptedWorker) ProcessNext(context.Context) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.invocations++

	if w.calls >= len(w.results) {
		// Сценарий исчерпан: отменяем контекст, чтобы цикл завершился.
		w.cancel()
		return false, context.Canceled
	}

	step := w.results[w.calls]
	w.calls++
	return step.progressed, step.err
}

func (w *scriptedWorker) counts() (calls, invocations int) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.calls, w.invocations
}

func runWorkerUntilDone(t *testing.T, steps []workerStep) (*scriptedWorker, string) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	worker := &scriptedWorker{results: steps, cancel: cancel}

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Паузы околонулевые: тест проверяет ветвление цикла, а не длительность
		// сна. Отдельно её проверяет TestWorkerStopsOnCancelDuringIdle.
		runMaterializeWorker(ctx, logger, worker, time.Millisecond, time.Millisecond)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("цикл воркера не завершился по отмене контекста")
	}

	return worker, logs.String()
}

// TestWorkerRunsWithoutPauseWhileProgressing — пока работа есть, цикл идёт без
// пауз: обход 50 000 customer (§13) иначе растянулся бы на часы ожидания.
func TestWorkerRunsWithoutPauseWhileProgressing(t *testing.T) {
	steps := make([]workerStep, 5)
	for i := range steps {
		steps[i] = workerStep{progressed: true}
	}

	started := time.Now()
	worker, _ := runWorkerUntilDone(t, steps)

	if calls, _ := worker.counts(); calls != len(steps) {
		t.Fatalf("шагов %d, ожидалось %d", calls, len(steps))
	}
	// Пауза между шагами прогресса сделала бы этот тест многосекундным.
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("цикл спал между шагами прогресса: %s", elapsed)
	}
}

// TestWorkerStopsOnCancelDuringIdle — отмена во время паузы обязана прерывать
// ожидание, а не досыпать его до конца.
func TestWorkerStopsOnCancelDuringIdle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Сценарий сам не отменяет: разбудить цикл обязана внешняя отмена, иначе
	// тест проверял бы не то.
	worker := &scriptedWorker{results: []workerStep{{progressed: false}}, cancel: func() {}}

	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))

	done := make(chan struct{})
	go func() {
		defer close(done)
		runMaterializeWorker(ctx, logger, worker, materializeIdleInterval, materializeErrorBackoff)
	}()

	// Первый шаг вернёт «работы нет», цикл уснёт на materializeIdleInterval;
	// отмена обязана разбудить его немедленно.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(materializeIdleInterval):
		t.Fatal("цикл досыпал паузу вместо немедленной остановки")
	}
}

// TestWorkerLogsFailureAndKeepsGoing — отказ шага не убивает воркер, но обязан
// попасть в лог как error: тихо остановившаяся материализация означала бы
// customer, навсегда оставшихся без новых access.
func TestWorkerLogsFailureAndKeepsGoing(t *testing.T) {
	worker, logs := runWorkerUntilDone(t, []workerStep{
		{err: errors.New("нет связи с базой")},
	})

	// Цикл вызвал воркер ещё раз после отказа — значит не остановился на нём.
	calls, invocations := worker.counts()
	if calls != 1 {
		t.Fatalf("израсходовано шагов %d, ожидался 1", calls)
	}
	if invocations < 2 {
		t.Fatalf("вызовов %d: отказ остановил воркер", invocations)
	}
	if !strings.Contains(logs, `"level":"ERROR"`) {
		t.Errorf("отказ шага не записан как error: %s", logs)
	}
	if !strings.Contains(logs, "нет связи с базой") {
		t.Errorf("в логе нет причины отказа: %s", logs)
	}
}

// TestWorkerCancellationIsNotAnError — отмена во время шага является штатной
// остановкой: иначе каждый рестарт пода писал бы в логи ложный отказ.
func TestWorkerCancellationIsNotAnError(t *testing.T) {
	_, logs := runWorkerUntilDone(t, nil)

	if strings.Contains(logs, `"level":"ERROR"`) {
		t.Errorf("штатная остановка записана как отказ: %s", logs)
	}
}
