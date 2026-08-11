// Команда spiritvpnd — backend SpiritVPN.
//
// Держит две поверхности: внешний customer gRPC API поверх mTLS (§5, §14) и
// служебный HTTP с health и readiness (§15). Порты разные намеренно — служебный
// наружу не публикуется, а liveness обязан отвечать даже когда gRPC не принимает
// соединения.
//
// Это composition root: единственное место, где конкретные адаптеры
// (postgres, crypto, grpcsvc) соединяются с use case'ами. Ниже по стеку
// зависимости приходят только через порты internal/app.
//
// Схему процесс не мигрирует: это отдельный шаг деплоя командой migrate (§11).
// Обнаружив схему старше своей, он остаётся not ready.
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
// Транзакции коротки по §11.1, а сетевых вызовов агентам в командном пути нет
// вовсе, поэтому текущие RPC завершаются за секунды. Запас нужен на случай
// зависшего запроса к базе: по его истечении соединения рвутся принудительно,
// потому что под, не уложившийся в grace period, всё равно будет убит SIGKILL.
const shutdownTimeout = 20 * time.Second

func main() {
	if err := run(); err != nil {
		// Логгер может быть ещё не собран: ошибка конфигурации случается раньше
		// него. Поэтому последнее слово процесса всегда идёт в stderr напрямую.
		fmt.Fprintln(os.Stderr, "spiritvpnd:", err)
		os.Exit(1)
	}
}

func run() error {
	// Сигналы перехватываются первыми: процесс обязан реагировать на SIGTERM уже
	// во время инициализации, иначе недоступная база сделает под неубиваемым до
	// конца grace period.
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
	// Самопроверка на старте, а не при первой команде: неработающий ключ обязан
	// проявиться до того, как процесс объявит себя готовым (§15).
	if err := cipher.SelfTest(); err != nil {
		return fmt.Errorf("самопроверка ключа шифрования: %w", err)
	}

	pool, err := newPool(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Реестр метрик собирается до use case'ов: их зависимости в него
	// заворачиваются (§15). Домен и app про Prometheus не знают — инструментируются
	// адаптеры, уже реализующие порты.
	registry := metrics.New()
	registry.RegisterPool(pool)

	// Версия миграций, зашитая в бинарь. Считается один раз: ошибка разбора имён
	// обязана валить старт, а не всплывать на первом probe.
	latest, err := migrations.Latest()
	if err != nil {
		return err
	}
	registry.SetBinarySchemaVersion(latest)

	// Один адаптер на оба use case: репозиторий владеет пулом, а командный и
	// read-путь отличаются только транзакцией, которую каждый открывает сам.
	repository := postgres.New(pool)

	// Расшифровки считаются на границе шифра, а не в местах вызова: Open —
	// единственная воронка обоих путей (§9 и §8), и считать по местам значило бы
	// забыть третье, когда оно появится.
	sealer := registry.WrapSealer(cipher)

	applyUC := app.NewApplyCustomerAccess(repository, crypto.NewGenerator(), sealer)
	linksUC := app.NewGetCustomerAccessLinks(repository, sealer)
	manifestUC := app.NewApplyFleetManifest(repository)

	owner := workerOwner()
	materializeUC := app.NewMaterializeManifest(
		repository, crypto.NewGenerator(), sealer, owner, materializeLeaseTTL)
	expiryUC := app.NewExpireCustomers(repository, crypto.NewGenerator())

	// Клиент агентов собирается здесь, потому что владеет соединениями: их надо
	// закрыть на выходе, а больше некому. TLS-материал читается сразу, поэтому
	// неверные пути к сертификатам валят старт, а не первую операцию.
	agentClient, err := nodeagent.New(nodeagent.Config{
		CertFile: cfg.Agent.CertFile,
		KeyFile:  cfg.Agent.KeyFile,
		CAFile:   cfg.Agent.CAFile,
	})
	if err != nil {
		return err
	}
	defer func() { _ = agentClient.Close() }()

	// Один декоратор на оба порта агента: диспетчер и pull worker ходят к нодам
	// через один и тот же клиент, и latency, коды исхода и health ноды снимаются
	// в одном месте, а не в двух воркерах по отдельности (§15).
	agent := registry.WrapAgent(agentClient)

	dispatchUC := app.NewDispatchOperations(
		repository, agent, sealer, processJitter{}, owner, dispatchLeaseTTL)
	usageUC := app.NewPullUsage(
		repository, agent, crypto.NewGenerator(), logger, owner, usageLeaseTTL, usagePullInterval)
	pruneUC := app.NewPruneUsageDedup(
		registry.WrapUsageRetention(repository), usageDedupRetention, usageDedupBatchSize)
	statsUC := registry.StatsWorker(repository)

	grpcServer, err := newGRPCServer(cfg.GRPC, logger, applyUC, linksUC, manifestUC)
	if err != nil {
		return err
	}

	checks := readinessChecks(pool, cipher, latest)

	// Фоновые воркеры живут на собственном контексте: serve может вернуться не
	// только по сигналу, но и по отказу слушателя, а ждать воркер, которому никто
	// не сказал остановиться, значило бы повесить процесс навсегда.
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

	// Expiry тоже в одном экземпляре, но по другой причине, чем материализация:
	// там курсор одной джобы, здесь параллелить просто нечего — шаг занимает
	// миллисекунды, а истекающих customer единицы в секунду (§13).
	start("expiry", expiryUC, expiryIdleInterval, expiryErrorBackoff)

	// Диспетчер параллелен, воркер материализации — нет: там шаг двигает общий
	// курсор одной джобы, здесь каждый шаг берёт свою операцию под SKIP LOCKED
	// (решения 35 и 39). Все восемь горутин делят один use case: состояния он не
	// держит, всё живёт в БД.
	for range dispatchConcurrency {
		start("dispatch", dispatchUC, dispatchIdleInterval, dispatchErrorBackoff)
	}

	// Опрос нод параллелен по той же причине, что и доставка: шаг упирается в
	// сетевой вызов, а на ноду гейт держит lease (§12, §13).
	for range dispatchConcurrency {
		start("usage", usageUC, usageIdleInterval, usageErrorBackoff)
	}

	// Ретенция реестра дедупа — единственный воркер без lease: удалять уже
	// удалённую строку нечего, поэтому конкуренция здесь стоит лишнего прохода, а
	// не корректности (§12).
	start("prune-usage-dedup", pruneUC, usageDedupIdleInterval, usageDedupErrorBackoff)

	// Снимок состояния для метрик — тоже воркер, но с постоянным темпом: его шаг
	// всегда сообщает «работы больше нет», поэтому цикл спит ровно idle-интервал
	// (§15). Один экземпляр: снимать одно и то же состояние параллельно незачем.
	start("stats", statsUC, statsRefreshInterval, statsErrorBackoff)

	err = serve(ctx, logger, cfg, grpcServer,
		newHTTPServer(cfg.HTTP.Listen, checks, registry.Handler()))

	// Воркеры останавливаются ПОСЛЕ поверхностей: их шаги коротки, а прогресс
	// зафиксирован в БД, поэтому ждать их безопасно и быстро. Операция, чей RPC
	// оборвала отмена, останется IN_FLIGHT и достанется сборщику протухших lease
	// (решение 51).
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
	// старта, а не асинхронным падением уже «поднявшегося» процесса.
	grpcListener, err := net.Listen("tcp", cfg.GRPC.Listen)
	if err != nil {
		return fmt.Errorf("прослушивание gRPC на %s: %w", cfg.GRPC.Listen, err)
	}

	httpListener, err := net.Listen("tcp", cfg.HTTP.Listen)
	if err != nil {
		_ = grpcListener.Close()
		return fmt.Errorf("прослушивание HTTP на %s: %w", cfg.HTTP.Listen, err)
	}

	// Буфер на оба отправителя: горутина не должна залипнуть на отправке, если
	// выход уже произошёл по другой ветке.
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

// shutdown завершает обе поверхности, не дожидаясь отменённого контекста
// процесса: он к этому моменту уже закрыт сигналом, а на корректное завершение
// текущих RPC нужен собственный бюджет времени.
func shutdown(logger *slog.Logger, grpcServer *grpc.Server, httpServer *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	stopped := make(chan struct{})
	go func() {
		// GracefulStop перестаёт принимать новые соединения и ждёт завершения
		// текущих RPC. Успешный Apply уже зафиксирован в PostgreSQL, поэтому
		// потерянным окажется максимум ответ — §16 описывает это как штатный
		// случай: точный повтор команды вернёт эквивалентное состояние.
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
