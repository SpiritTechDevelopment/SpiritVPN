// Команда spiritvpnd — backend SpiritVPN.
//
// Процесс держит две сетевые поверхности:
//
//   - внешнюю — gRPC поверх mTLS, через неё приходят команды Customer Service и
//     infrastructure pipeline;
//   - служебную — HTTP с /health/live, /health/ready и /metrics на отдельном
//     порту. Наружу не публикуется, на liveness отвечает и тогда, когда gRPC не
//     принимает соединения.
//
// Здесь же composition root: адаптеры (postgres, crypto, nodeagent, grpcsvc)
// подставляются в use case'ы, а ниже по стеку зависимости приходят только через
// порты internal/app.
//
// Схему процесс не мигрирует, её накатывает команда migrate отдельным шагом
// деплоя. На схеме старше встроенной в бинарь процесс остаётся not ready.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/config"
	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	"github.com/RomanRyabinkin/SpiritVPN/internal/metrics"
	"github.com/RomanRyabinkin/SpiritVPN/internal/migrations"
	"github.com/RomanRyabinkin/SpiritVPN/internal/nodeagent"
	"github.com/RomanRyabinkin/SpiritVPN/internal/postgres"
)

// shutdownTimeout ограничивает graceful shutdown.
//
// Текущие RPC укладываются в секунды: транзакции командного пути коротки, а
// походов к агентам в нём нет. Бюджет рассчитан на зависший запрос к базе. По
// его истечении соединения рвутся принудительно — под, не уложившийся в grace
// period, всё равно получит SIGKILL.
const shutdownTimeout = 20 * time.Second

func main() {
	if err := run(); err != nil {
		// Логгер собирается после разбора конфигурации, поэтому ошибка старта
		// пишется в stderr напрямую.
		fmt.Fprintln(os.Stderr, "spiritvpnd:", err)
		os.Exit(1)
	}
}

func run() error {
	// Перехват сигналов стоит до инициализации, чтобы SIGTERM прерывал и её тоже.
	// Процесс, застрявший на недоступной базе, иначе висит до конца grace period.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}

	logger := newLogger(cfg.Log.Level)
	logger.LogAttrs(ctx, slog.LevelInfo, "конфигурация загружена", slog.Any("config", cfg))

	cipher, err := crypto.NewCipher(cfg.ClientUUIDKey)
	if err != nil {
		return fmt.Errorf("шифр client_uuid: %w", err)
	}
	// Самопроверка отсекает неработающий ключ до того, как процесс объявит себя
	// готовым. Та же проверка стоит в readiness.
	if err := cipher.SelfTest(); err != nil {
		return fmt.Errorf("самопроверка ключа шифрования: %w", err)
	}

	pool, err := newPool(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Реестр собирается до use case'ов, потому что заворачивает их зависимости.
	// WrapSealer и WrapAgent ниже возвращают те же порты со съёмом метрик; домен
	// и app про Prometheus не знают.
	registry := metrics.New()
	registry.RegisterPool(pool)

	// Версия миграций, зашитая в бинарь. Считается один раз на два потребителя:
	// readiness сверяет с ней схему в базе, метрика binary_schema_version
	// публикует её.
	latest, err := migrations.Latest()
	if err != nil {
		return err
	}
	registry.SetBinarySchemaVersion(latest)

	// Один Repository на все репозиторные порты. Он владеет пулом, а пути
	// различаются только транзакцией, которую каждый открывает сам.
	repository := postgres.New(pool)

	// Счётчик расшифровок снимается на границе шифра. Через Open проходят и
	// диспетчер, и выдача ссылок, и всякий следующий потребитель; съём по местам
	// вызова пришлось бы дописывать к каждому новому.
	sealer := registry.WrapSealer(cipher)

	applyUC := app.NewApplyCustomerAccess(repository, crypto.NewGenerator(), sealer)
	linksUC := app.NewGetCustomerAccessLinks(repository, sealer)
	manifestUC := app.NewApplyFleetManifest(repository)

	owner := workerOwner()
	materializeUC := app.NewMaterializeManifest(
		repository, crypto.NewGenerator(), sealer, owner, materializeLeaseTTL)
	expiryUC := app.NewExpireCustomers(repository, crypto.NewGenerator())

	// Клиент агентов владеет соединениями и закрывает их на выходе, поэтому
	// собирается здесь. TLS-материал читается сразу, так что неверные пути к
	// сертификатам валят старт, а не первую операцию.
	agentClient, err := nodeagent.New(nodeagent.Config{
		CertFile: cfg.Agent.CertFile,
		KeyFile:  cfg.Agent.KeyFile,
		CAFile:   cfg.Agent.CAFile,
	})
	if err != nil {
		return err
	}
	defer func() { _ = agentClient.Close() }()

	// Один декоратор на оба порта агента. Диспетчер, опрос трафика и reconcile
	// ходят к нодам через один клиент, поэтому latency, коды исхода и состояние
	// ноды снимаются в одном месте на все три воркера.
	agent := registry.WrapAgent(agentClient)

	dispatchUC := app.NewDispatchOperations(
		repository, agent, sealer, processJitter{}, owner, dispatchLeaseTTL)
	usageUC := app.NewPullUsage(
		repository, agent, crypto.NewGenerator(), logger, owner, usageLeaseTTL, usagePullInterval)
	reconcileUC := app.NewReconcileNodes(
		repository, agent, sealer, crypto.NewGenerator(), app.SystemClock{}, logger,
		owner, reconcileLeaseTTL, reconcileInterval, maxObservationAge)
	pruneUC := app.NewPruneUsageDedup(
		registry.WrapUsageRetention(repository), usageDedupRetention, usageDedupBatchSize)
	statsUC := registry.StatsWorker(repository)

	grpcServer, err := newGRPCServer(cfg.GRPC, logger, applyUC, linksUC, manifestUC)
	if err != nil {
		return err
	}

	checks := readinessChecks(pool, cipher, latest)

	// Фоновые воркеры живут на собственном контексте. serve возвращается и по
	// отказу слушателя, не только по сигналу, а workers.Wait ниже без явной
	// отмены ждал бы вечно.
	workerCtx, stopWorkers := context.WithCancel(ctx)
	defer stopWorkers()

	var workers sync.WaitGroup
	start := func(name string, uc stepWorker, idle, backoff time.Duration) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			runWorker(workerCtx, logger, name, uc, idle, backoff)
		}()
	}

	start("materialize", materializeUC, materializeIdleInterval, materializeErrorBackoff)

	// Expiry в одном экземпляре. Шаг занимает миллисекунды при единицах
	// истекающих customer в секунду. Координация у него — FOR UPDATE SKIP LOCKED
	// на строке customer, ни джобы, ни lease нет.
	start("expiry", expiryUC, expiryIdleInterval, expiryErrorBackoff)

	// Диспетчер параллелен, в отличие от воркера материализации: там шаг двигает
	// общий курсор одной джобы, здесь каждый шаг берёт свою операцию под SKIP
	// LOCKED. Все восемь горутин делят один use case, состояния он не держит,
	// всё живёт в БД. Предел «одна операция на ноду одновременно» держит
	// частичный уникальный индекс, а не число горутин.
	for range dispatchConcurrency {
		start("dispatch", dispatchUC, dispatchIdleInterval, dispatchErrorBackoff)
	}

	// Опрос нод параллелен по той же причине, что и доставка. Шаг упирается в
	// сетевой вызов, а вторую горутину на ту же ноду не пускает её lease.
	for range dispatchConcurrency {
		start("usage", usageUC, usageIdleInterval, usageErrorBackoff)
	}

	// Reconcile в одном экземпляре. Его шаг тоже упирается в сетевой вызов, но
	// обходит он весь fleet по таймеру, а не очередь: десяток нод с интервалом
	// созревания в пять минут одна горутина проходит без отставания. На саму
	// ноду вторую сверку не пускает её lease.
	start("reconcile", reconcileUC, reconcileIdleInterval, reconcileErrorBackoff)

	// Ретенция реестра дедупа обходится без координации. Повторное удаление уже
	// удалённой строки безвредно, и гонка здесь стоит лишнего прохода.
	start("prune-usage-dedup", pruneUC, usageDedupIdleInterval, usageDedupErrorBackoff)

	// Снимок состояния для метрик идёт постоянным темпом. Его шаг всегда
	// сообщает «работы больше нет», и цикл спит ровно idle-интервал. Экземпляр
	// один, параллельный съём того же состояния ничего не добавляет.
	start("stats", statsUC, statsRefreshInterval, statsErrorBackoff)

	err = serve(ctx, logger, cfg, grpcServer,
		newHTTPServer(cfg.HTTP.Listen, checks, registry.Handler()))

	// Воркеры останавливаются после того, как обе поверхности закрыты. Их шаги
	// коротки, а прогресс зафиксирован в БД, так что ожидание короткое.
	// Операция, чей RPC оборвала отмена, останется IN_FLIGHT и достанется
	// сборщику протухших lease.
	stopWorkers()
	workers.Wait()

	return err
}

// serve поднимает обе поверхности и ждёт либо сигнала, либо отказа одной из них.
func serve(
	ctx context.Context,
	logger *slog.Logger,
	cfg config.Config,
	grpcServer *grpc.Server,
	httpServer *http.Server,
) error {
	// Слушатели открываются до запуска горутин, чтобы занятый порт стал ошибкой
	// старта, а не отказом секундой позже записи «слушаю».
	grpcListener, err := net.Listen("tcp", cfg.GRPC.Listen)
	if err != nil {
		return fmt.Errorf("прослушивание gRPC на %s: %w", cfg.GRPC.Listen, err)
	}

	httpListener, err := net.Listen("tcp", cfg.HTTP.Listen)
	if err != nil {
		_ = grpcListener.Close()
		return fmt.Errorf("прослушивание HTTP на %s: %w", cfg.HTTP.Listen, err)
	}

	// Буфер на оба отправителя. Выход по другой ветке select оставляет отправку
	// без читателя, и на небуферизованном канале горутина осталась бы висеть.
	failed := make(chan error, 2)

	go func() {
		failed <- grpcServer.Serve(grpcListener)
	}()
	go func() {
		if err := httpServer.Serve(httpListener); !errors.Is(err, http.ErrServerClosed) {
			failed <- err
		}
	}()

	logger.LogAttrs(ctx, slog.LevelInfo, "слушаю",
		slog.String("grpc", cfg.GRPC.Listen),
		slog.String("http", cfg.HTTP.Listen),
	)

	var runErr error
	select {
	case runErr = <-failed:
		logger.LogAttrs(ctx, slog.LevelError, "поверхность отказала", slog.Any("error", runErr))
	case <-ctx.Done():
		logger.LogAttrs(ctx, slog.LevelInfo, "получен сигнал, останавливаюсь")
	}

	shutdown(logger, grpcServer, httpServer)
	return runErr
}

// shutdown завершает обе поверхности на собственном контексте. Контекст
// процесса к этому моменту уже отменён сигналом, а на корректное завершение
// текущих RPC нужен свой бюджет времени.
func shutdown(logger *slog.Logger, grpcServer *grpc.Server, httpServer *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	stopped := make(chan struct{})
	go func() {
		// GracefulStop перестаёт принимать новые соединения и ждёт завершения
		// текущих RPC. Успешный Apply уже зафиксирован в PostgreSQL, так что при
		// обрыве теряется максимум ответ: точный повтор команды вернёт
		// эквивалентное состояние.
		grpcServer.GracefulStop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-ctx.Done():
		logger.LogAttrs(context.Background(), slog.LevelWarn,
			"graceful shutdown не уложился в бюджет, рву соединения",
			slog.Duration("timeout", shutdownTimeout))
		grpcServer.Stop()
	}

	if err := httpServer.Shutdown(ctx); err != nil {
		logger.LogAttrs(context.Background(), slog.LevelWarn,
			"HTTP не завершился корректно", slog.Any("error", err))
	}
}
