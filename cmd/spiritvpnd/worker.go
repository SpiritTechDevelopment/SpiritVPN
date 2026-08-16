package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"time"
)

// Темп воркера материализации.
//
// Константы, а не конфигурация: ручка без потребителя даёт лишний способ
// выставить значение, которое никто не проверял нагрузкой.
const (
	// materializeIdleInterval — пауза, когда джоб нет. Манифест применяет CI/CD,
	// это редкая операция.
	materializeIdleInterval = 5 * time.Second

	// materializeErrorBackoff — пауза после отказа базы. Больше idle, чтобы цикл
	// не крутился на полной скорости, пока база недоступна.
	materializeErrorBackoff = 15 * time.Second

	// materializeLeaseTTL — на сколько воркер берёт джобу. С запасом перекрывает
	// один шаг (одна короткая транзакция) и ограничивает простой джобы после
	// падения процесса.
	materializeLeaseTTL = 60 * time.Second
)

// Темп и параллелизм диспетчера операций.
const (
	// dispatchConcurrency — число горутин доставки. На одну ноду при любом их
	// числе уходит не более одной операции: гейт держит частичный уникальный
	// индекс в SQL.
	dispatchConcurrency = 8

	// dispatchIdleInterval — пауза, когда очередь пуста. Короче, чем у
	// материализации: операция появляется на каждую команду customer, и лишняя
	// секунда ожидания здесь — секунда неработающей ссылки.
	dispatchIdleInterval = time.Second

	// dispatchErrorBackoff — пауза после отказа базы. Отказ агента сюда не
	// доходит: он записывается как исход операции со своим backoff.
	dispatchErrorBackoff = 15 * time.Second

	// dispatchLeaseTTL — на сколько берётся операция. С запасом перекрывает
	// deadline вызова (nodeagent.DefaultCallTimeout, 5 секунд): lease, истёкший
	// раньше ответа агента, дал бы вторую параллельную операцию на ту же ноду.
	dispatchLeaseTTL = 30 * time.Second
)

// Темп воркера истечения.
const (
	// expiryIdleInterval — пауза, когда гасить некого. Потолок задержки гашения —
	// 10 секунд, пять оставляют запас на затянувшийся шаг.
	expiryIdleInterval = 5 * time.Second

	// expiryErrorBackoff — пауза после отказа базы, как и у остальных воркеров.
	expiryErrorBackoff = 15 * time.Second
)

// Темп воркера учёта трафика.
const (
	// usagePullInterval — как часто опрашивается одна нода. Агент снимает
	// счётчики Xray каждые 15 секунд и на более частый запрос вернёт пустой
	// ответ.
	usagePullInterval = 15 * time.Second

	// usageIdleInterval — пауза, когда ни одна нода не «созрела». Короче
	// интервала опроса: восемь воркеров делят все ноды, и на более редких
	// пробуждениях темп опроса просел бы ниже заявленного.
	usageIdleInterval = 3 * time.Second

	// usageErrorBackoff — пауза после отказа базы. Недоступность агента сюда не
	// доходит, она не ошибка шага.
	usageErrorBackoff = 15 * time.Second

	// usageLeaseTTL — на сколько берётся нода. С запасом перекрывает
	// nodeagent.DefaultPullTimeout плюс транзакции всех групп batch: истёкший
	// раньше времени lease пустил бы на ту же ноду второй опрос.
	usageLeaseTTL = 5 * time.Minute
)

// Темп authoritative reconcile.
const (
	// reconcileInterval — как часто нода приводится к desired state по таймеру.
	//
	// Изменения по одному везёт диспетчер, а reconcile существует ради
	// инициализации и починки. Величина задаёт, как часто нода сверяется с
	// фактическим инвентарём, и она же потолок жизни расхождения, о котором никто
	// не сообщил.
	//
	// Для масштаба: usage-воркер ходит на ту же ноду 240 раз в час, reconcile —
	// двенадцать.
	reconcileInterval = 5 * time.Minute

	// maxObservationAge — предел давности снимка Xray, с которым можно сверяться.
	//
	// Вдвое больше интервала: снимок делает агент своим темпом, и одна
	// пропущенная им итерация не должна отменять сверку. Сверху величину
	// ограничивает риск чинить ноду по давно неверному состоянию, снизу — ровно
	// интервал, на котором любое расхождение темпов гасило бы сверку насовсем.
	maxObservationAge = 2 * reconcileInterval

	// reconcileIdleInterval — пауза, когда ни одна нода не «созрела». Короче
	// интервала сверки: воркер один на весь fleet, и на более редких
	// пробуждениях обход отставал бы от заявленного темпа.
	reconcileIdleInterval = 15 * time.Second

	// reconcileErrorBackoff — пауза после отказа базы. Недоступность агента сюда
	// не доходит, она не ошибка шага.
	reconcileErrorBackoff = 15 * time.Second

	// reconcileLeaseTTL — на сколько берётся нода. С запасом перекрывает
	// nodeagent.DefaultReconcileTimeout: истёкший раньше ответа lease пустил бы
	// второй полный набор на ту же ноду.
	reconcileLeaseTTL = 5 * time.Minute
)

// Ретенция реестра дедупа usage-items.
const (
	// usageDedupRetention — сколько дедуп-запись живёт после того, как её batch
	// подтверждён.
	//
	// Величина задаёт не запас к темпу агента (он чистит спул на следующем же
	// опросе, через 15 секунд), а длину простоя backend, которую удаётся пережить
	// без двойного начисления: пока backend лежит, подтверждение до ноды не
	// доехало, и агент пришлёт тот же batch снова. Шесть часов покрывают ночную
	// аварию до утра. Простой длиннее окна начислит трафик повторно — это
	// принятая положительная погрешность, закрытая fault-тестом.
	//
	// Вверх окно упирается в размер таблицы: она растёт на строку на активного
	// пользователя каждые 15 секунд, примерно на 5760 строк в сутки на
	// пользователя.
	usageDedupRetention = 6 * time.Hour

	// usageDedupBatchSize — потолок удаления за один шаг. Он же порог «работа не
	// закончена»: шаг, удаливший ровно потолок, идёт на следующий без паузы.
	// Значение подобрано так, чтобы шаг оставался коротким на любой таблице.
	usageDedupBatchSize = 5000

	// usageDedupIdleInterval — пауза, когда удалять нечего. Минуты, а не секунды:
	// строки становятся годными к удалению не быстрее, чем истекает окно
	// ретенции.
	usageDedupIdleInterval = 5 * time.Minute

	// usageDedupErrorBackoff — пауза после отказа базы, как у остальных воркеров.
	usageDedupErrorBackoff = 15 * time.Second
)

// Темп снимка состояния для метрик.
const (
	// statsRefreshInterval — как часто снимается состояние БД. Совпадает с
	// типичным scrape interval Prometheus: на более частых снимках лишние
	// значения никто не прочитает, на более редких метрики отстают от того, на
	// что смотрит оператор.
	//
	// Он же задаёт возраст выдачи /metrics: снимок читается из памяти, и значения
	// устаревают ровно на этот интервал.
	statsRefreshInterval = 15 * time.Second

	// statsErrorBackoff — пауза после отказа базы, как у остальных воркеров.
	statsErrorBackoff = 15 * time.Second
)

// stepWorker — то, что цикл требует от воркера: один шаг, сообщающий, была ли
// работа. Такую форму имеют все фоновые воркеры, поэтому цикл у них общий.
type stepWorker interface {
	ProcessNext(ctx context.Context) (bool, error)
}

// workerOwner идентифицирует процесс в lease_owner: по нему видно, чей lease
// протух.
func workerOwner() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return fmt.Sprintf("%s/%d", host, os.Getpid())
}

// runWorker крутит шаги фонового воркера до отмены контекста.
//
// Шаг маленький (один customer у материализации, одна операция у диспетчера),
// поэтому цикл сам решает, когда спать: пока есть прогресс — идёт без пауз, на
// пустом проходе ждёт. Между шагами нет незавершённого состояния, весь прогресс
// зафиксирован в БД, так что остановка отрабатывает мгновенно в любой момент.
//
// Паузы приходят параметрами, а не берутся из констант напрямую, чтобы тест
// поведения цикла не ждал реальных секунд.
//
// name попадает в лог отказа: горутин у диспетчера восемь, и без имени по
// записи не понять, какой воркер отказал.
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

// processJitter — источник случайности для backoff повторов.
//
// math/rand/v2, а не crypto/rand: значение разводит попытки во времени и
// секретности не несёт, а отказ CSPRNG не должен ронять доставку.
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
