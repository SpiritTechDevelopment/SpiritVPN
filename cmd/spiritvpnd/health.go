package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
)

// readinessTimeout ограничивает суммарную проверку готовности: probe, который
// не отвечает, kubelet трактует как отказ, поэтому лучше ответить «не готов»
// быстро и самому.
const readinessTimeout = 3 * time.Second

// readinessCheck — одна именованная проверка готовности.
type readinessCheck struct {
	name string
	run  func(context.Context) error
}

// newHTTPServer собирает служебную поверхность.
//
// Все три endpoint'а живут на служебном порту и наружу не публикуются. Для
// /metrics это существенно, а не наследование: выдача раскрывает размер fleet,
// объём трафика и состав очередей. Аутентификации у него нет — её для
// служебного порта не требует, и границей служит сеть.
func newHTTPServer(addr string, checks []readinessCheck, metrics http.Handler) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", handleLive)
	mux.HandleFunc("GET /health/ready", handleReady(checks))
	mux.Handle("GET /metrics", metrics)

	return &http.Server{
		Addr:    addr,
		Handler: mux,

		// Соединение, не приславшее заголовки, не должно держать воркер вечно.
		ReadHeaderTimeout: 5 * time.Second,
	}
}

// handleLive отвечает всегда.
//
// Liveness не зависит ни от PostgreSQL, ни от нод. Проверка базы здесь
// означала бы, что кратковременная недоступность БД приводит к перезапуску всех
// подов сразу — то есть превращает деградацию в полный отказ.
func handleLive(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// handleReady требует доступную БД, совместимую схему и валидный ключ.
func handleReady(checks []readinessCheck) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()

		for _, check := range checks {
			if err := check.run(ctx); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)

				// Наружу уходит только имя проверки. Текст ошибки pgx содержит
				// параметры подключения, а этот endpoint по определению доступен
				// всему, что дотянулось до служебного порта.
				_, _ = fmt.Fprintf(w, "not ready: %s\n", check.name)
				return
			}
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	}
}

// readinessChecks собирает проверки в порядке возрастания стоимости: смысла
// спрашивать схему у недоступной базы нет.
//
// latest приходит параметром, а не читается здесь: ту же версию публикует
// метрика binary_schema_version, и вычислять её дважды значило бы завести
// два источника одного числа.
func readinessChecks(pool *pgxpool.Pool, cipher *crypto.Cipher, latest uint) []readinessCheck {
	return []readinessCheck{
		{
			name: "postgres",
			run:  pool.Ping,
		},
		{
			name: "schema",
			run:  schemaCheck(pool, latest),
		},
		{
			name: "encryption_key",
			run:  func(context.Context) error { return cipher.SelfTest() },
		},
	}
}

// schemaCheck сверяет версию схемы с миграциями, встроенными в этот бинарь.
//
// Условие «не ниже», а не «равно», намеренно: при rollout миграции накатываются
// до выкатки новых подов, и старый бинарь какое-то время живёт с более новой
// схемой. Требование точного равенства сделало бы его not ready и остановило бы
// обслуживание ровно в момент деплоя.
func schemaCheck(pool *pgxpool.Pool, latest uint) func(context.Context) error {
	return func(ctx context.Context) error {
		var (
			version int64
			dirty   bool
		)

		err := pool.QueryRow(ctx, "SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty)
		if err != nil {
			return fmt.Errorf("версия схемы: %w", err)
		}

		// dirty означает прерванную миграцию: схема в неизвестном состоянии, и
		// принимать на ней команды нельзя.
		if dirty {
			return fmt.Errorf("схема dirty на версии %d", version)
		}

		if version < int64(latest) {
			return fmt.Errorf("схема версии %d, бинарю нужна не ниже %d", version, latest)
		}

		return nil
	}
}
