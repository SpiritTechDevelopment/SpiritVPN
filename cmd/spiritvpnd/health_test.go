package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RomanRyabinkin/SpiritVPN/internal/metrics"
)

func okCheck(name string) readinessCheck {
	return readinessCheck{name: name, run: func(context.Context) error { return nil }}
}

func failingCheck(name string, err error) readinessCheck {
	return readinessCheck{name: name, run: func(context.Context) error { return err }}
}

// healthServer собирает служебную поверхность для тестов health.
//
// Вместо реестра метрик — заглушка: /metrics проверяется отдельно, а health к
// нему не обращается.
func healthServer(checks ...readinessCheck) *http.Server {
	return newHTTPServer(":0", checks, http.NotFoundHandler())
}

func do(t *testing.T, server *http.Server, path string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// TestLivenessIgnoresDependencies — §15: liveness не зависит ни от PostgreSQL,
// ни от нод. Проверка базы здесь превратила бы её кратковременную недоступность
// в одновременный перезапуск всех подов.
func TestLivenessIgnoresDependencies(t *testing.T) {
	server := healthServer(
		failingCheck("postgres", errors.New("база недоступна")),
	)

	if rec := do(t, server, "/health/live"); rec.Code != http.StatusOK {
		t.Errorf("код %d, ожидался 200 несмотря на упавшую зависимость", rec.Code)
	}
}

func TestReadinessPassesWhenAllChecksPass(t *testing.T) {
	server := healthServer(
		okCheck("postgres"), okCheck("schema"), okCheck("encryption_key"),
	)

	rec := do(t, server, "/health/ready")
	if rec.Code != http.StatusOK {
		t.Errorf("код %d, ожидался 200", rec.Code)
	}
}

func TestReadinessFailsOnFirstFailingCheck(t *testing.T) {
	server := healthServer(
		okCheck("postgres"),
		failingCheck("schema", errors.New("схема версии 0, бинарю нужна не ниже 1")),
		okCheck("encryption_key"),
	)

	rec := do(t, server, "/health/ready")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("код %d, ожидался 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "schema") {
		t.Errorf("тело %q не называет упавшую проверку", rec.Body.String())
	}
}

// TestReadinessHidesCheckDetails — служебный порт доступен всему, что до него
// дотянулось, поэтому наружу уходит только имя проверки: текст ошибки pgx
// содержит параметры подключения.
func TestReadinessHidesCheckDetails(t *testing.T) {
	const detail = "host=db.internal user=spirit password=hunter2"

	server := healthServer(
		failingCheck("postgres", errors.New(detail)),
	)

	body := do(t, server, "/health/ready").Body.String()
	for _, leaked := range []string{"db.internal", "hunter2", "password"} {
		if strings.Contains(body, leaked) {
			t.Errorf("тело %q раскрывает %q", body, leaked)
		}
	}
}

// TestMetricsServesRegistry — §15 требует /metrics на служебном порту рядом с
// health. Проверяется именно проводка: что endpoint зарегистрирован и отдаёт
// переданный реестр, а не пустой ответ и не глобальный DefaultRegisterer.
func TestMetricsServesRegistry(t *testing.T) {
	server := newHTTPServer(":0", nil, metrics.New().Handler())

	rec := do(t, server, "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d, ожидался 200", rec.Code)
	}

	// Метрика из снимка БД: она объявлена в реестре с предзаполнением нулями,
	// поэтому присутствует и до первого снимка.
	if !strings.Contains(rec.Body.String(), "spiritvpn_agent_operations") {
		t.Error("в выдаче нет метрик SpiritVPN")
	}
}

// TestMetricsIsNotServedOnHealthPaths — регрессионный guard: обработчик метрик
// зарегистрирован на своём пути, а не поверх health. Перехватив /health/live, он
// сделал бы liveness зависящим от реестра.
func TestMetricsIsNotServedOnHealthPaths(t *testing.T) {
	server := newHTTPServer(":0", nil, metrics.New().Handler())

	if body := do(t, server, "/health/live").Body.String(); strings.Contains(body, namespacePrefix) {
		t.Errorf("liveness отдаёт метрики: %q", body)
	}
}

// namespacePrefix — префикс метрик SpiritVPN. Дублирует внутреннюю константу
// пакета metrics намеренно: тест обязан заметить её изменение, а не следовать
// за ним.
const namespacePrefix = "spiritvpn_"

// TestReadinessStopsAtFirstFailure — проверки идут по возрастанию стоимости, и
// спрашивать схему у недоступной базы незачем.
func TestReadinessStopsAtFirstFailure(t *testing.T) {
	reachedSchema := false

	server := healthServer(
		failingCheck("postgres", errors.New("недоступна")),
		readinessCheck{name: "schema", run: func(context.Context) error {
			reachedSchema = true
			return nil
		}},
	)

	do(t, server, "/health/ready")

	if reachedSchema {
		t.Error("проверка схемы выполнена, хотя база уже признана недоступной")
	}
}
