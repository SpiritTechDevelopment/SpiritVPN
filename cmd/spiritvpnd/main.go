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

	// Один адаптер на оба use case: репозиторий владеет пулом, а командный и
	// read-путь отличаются только транзакцией, которую каждый открывает сам.
	repository := postgres.New(pool)
	applyUC := app.NewApplyCustomerAccess(repository, crypto.NewGenerator(), cipher)
	linksUC := app.NewGetCustomerAccessLinks(repository, cipher)
	manifestUC := app.NewApplyFleetManifest(repository)
	materializeUC := app.NewMaterializeManifest(
		repository, crypto.NewGenerator(), cipher, workerOwner(), materializeLeaseTTL)

	grpcServer, err := newGRPCServer(cfg.GRPC, logger, applyUC, linksUC, manifestUC)
	if err != nil {
		return err
	}

	checks, err := readinessChecks(pool, cipher)
	if err != nil {
		return err
	}

	// Фоновый воркер живёт на собственном контексте: serve может вернуться не
	// только по сигналу, но и по отказу слушателя, а ждать воркер, которому никто
	// не сказал остановиться, значило бы повесить процесс навсегда.
	workerCtx, stopWorkers := context.WithCancel(ctx)
	defer stopWorkers()

	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		runMaterializeWorker(workerCtx, logger, materializeUC,
			materializeIdleInterval, materializeErrorBackoff)
	}()

	err = serve(ctx, logger, cfg, grpcServer, newHTTPServer(cfg.HTTP.Listen, checks))

	// Воркер останавливается ПОСЛЕ поверхностей: его шаг короток, а прогресс
	// зафиксирован курсором, поэтому ждать его безопасно и быстро.
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
