package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// Темп воркера материализации (§13).
//
// Константы, а не конфигурация: ручка без потребителя — лишний способ выставить
// значение, которое никто не проверял нагрузкой (то же соображение, что и в
// решении 26). Здесь же живёт shutdownTimeout.
const (
	// materializeIdleInterval — пауза, когда джоб нет. Манифест применяет CI/CD,
	// это редкая операция, поэтому опрашивать чаще незачем.
	materializeIdleInterval = 5 * time.Second

	// materializeErrorBackoff — пауза после отказа. Заметно больше idle: если
	// база недоступна, крутить цикл на полной скорости бессмысленно и вредно.
	materializeErrorBackoff = 15 * time.Second

	// materializeLeaseTTL — на сколько воркер берёт джобу. Должен с запасом
	// перекрывать один шаг (одна короткая транзакция), но не быть настолько
	// большим, чтобы после падения реплики джоба простаивала минутами (§13).
	materializeLeaseTTL = 60 * time.Second
)

// materializeUseCase — то, что цикл требует от воркера.
type materializeUseCase interface {
	ProcessNext(ctx context.Context) (bool, error)
}

// workerOwner идентифицирует реплику в lease_owner (§13, §15): по нему видно,
// чей lease протух.
func workerOwner() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return fmt.Sprintf("%s/%d", host, os.Getpid())
}

// runMaterializeWorker крутит шаги материализации до отмены контекста (§13).
//
// Шаг маленький (один customer), поэтому цикл сам решает, когда спать: пока есть
// прогресс — идёт без пауз, на пустом проходе ждёт. Такая форма делает
// остановку мгновенной в любой момент: между шагами нет незавершённого
// состояния, весь прогресс уже зафиксирован курсором в БД.
//
// Паузы приходят параметрами, а не берутся из констант напрямую: иначе тест
// поведения цикла пришлось бы ждать реальные секунды, и его бы просто не было.
func runMaterializeWorker(
	ctx context.Context,
	logger *slog.Logger,
	uc materializeUseCase,
	idleInterval, errorBackoff time.Duration,
) {
	for {
		progressed, err := uc.ProcessNext(ctx)

		switch {
		case err != nil && ctx.Err() != nil:
			// Отмена во время шага — это штатная остановка, а не отказ.
			// Незакоммиченная транзакция откатится сама.
			return

		case err != nil:
			logger.LogAttrs(ctx, slog.LevelError, "шаг материализации манифеста отказал",
				slog.Any("error", err))
			if !sleepOrDone(ctx, errorBackoff) {
				return
			}

		case progressed:
			// Работа есть — сразу следующий customer, без паузы.

		default:
			if !sleepOrDone(ctx, idleInterval) {
				return
			}
		}

		if ctx.Err() != nil {
			return
		}
	}
}

// sleepOrDone ждёт либо истечения паузы, либо отмены. false означает отмену.
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
