package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
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

// Темп и параллелизм диспетчера операций (§9, §13).
const (
	// dispatchConcurrency — число горутин доставки. Дефолт §13; на одну ноду
	// по-прежнему уходит не более одной операции, и этот гейт держится в SQL, а
	// не числом горутин (решение 39).
	dispatchConcurrency = 8

	// dispatchIdleInterval — пауза, когда очередь пуста. Заметно короче
	// материализации: операция появляется на каждую команду customer, и лишняя
	// секунда ожидания здесь — это секунда неработающей ссылки.
	dispatchIdleInterval = time.Second

	// dispatchErrorBackoff — пауза после отказа БАЗЫ. Отказ агента сюда не
	// доходит: он не ошибка шага, а записанный исход операции со своим backoff
	// (§9).
	dispatchErrorBackoff = 15 * time.Second

	// dispatchLeaseTTL — на сколько берётся операция. С запасом перекрывает
	// deadline вызова (nodeagent.DefaultCallTimeout, 5 секунд): lease, истёкший
	// раньше ответа агента, дал бы вторую параллельную операцию на ту же ноду
	// вопреки §9.
	dispatchLeaseTTL = 30 * time.Second
)

// Темп воркера истечения (§13).
const (
	// expiryIdleInterval — пауза, когда гасить некого. §13 требует запускать
	// воркер не реже раза в 10 секунд; пять оставляют запас на затянувшийся шаг.
	expiryIdleInterval = 5 * time.Second

	// expiryErrorBackoff — пауза после отказа базы, как и у остальных воркеров.
	expiryErrorBackoff = 15 * time.Second
)

// stepWorker — то, что цикл требует от воркера: один шаг, сообщающий, была ли
// работа. Такую форму имеют оба фоновых воркера, поэтому цикл у них общий.
type stepWorker interface {
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

// runWorker крутит шаги фонового воркера до отмены контекста (§9, §13).
//
// Шаг маленький (один customer у материализации, одна операция у диспетчера),
// поэтому цикл сам решает, когда спать: пока есть прогресс — идёт без пауз, на
// пустом проходе ждёт. Такая форма делает остановку мгновенной в любой момент:
// между шагами нет незавершённого состояния, весь прогресс уже зафиксирован в БД.
//
// Паузы приходят параметрами, а не берутся из констант напрямую: иначе тест
// поведения цикла пришлось бы ждать реальные секунды, и его бы просто не было.
//
// name попадает в лог отказа: горутин у диспетчера восемь, и без имени в записи
// невозможно понять, какой воркер отказал.
func runWorker(
	ctx context.Context,
	logger *slog.Logger,
	name string,
	uc stepWorker,
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
			logger.LogAttrs(ctx, slog.LevelError, "шаг воркера отказал",
				slog.String("worker", name),
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

// processJitter — источник случайности для backoff повторов (§9).
//
// math/rand/v2, а не crypto/rand: значение разводит попытки во времени и никакой
// секретности не несёт, а отказ CSPRNG не должен уметь провалить доставку.
// Собственного пакета не заводит — это одна функция.
type processJitter struct{}

func (processJitter) Unit() float64 { return rand.Float64() }

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
