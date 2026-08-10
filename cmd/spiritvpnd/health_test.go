package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func okCheck(name string) readinessCheck {
	return readinessCheck{name: name, run: func(context.Context) error { return nil }}
}

func failingCheck(name string, err error) readinessCheck {
	return readinessCheck{name: name, run: func(context.Context) error { return err }}
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
	server := newHTTPServer(":0", []readinessCheck{
		failingCheck("postgres", errors.New("база недоступна")),
	})

	if rec := do(t, server, "/health/live"); rec.Code != http.StatusOK {
		t.Errorf("код %d, ожидался 200 несмотря на упавшую зависимость", rec.Code)
	}
}

func TestReadinessPassesWhenAllChecksPass(t *testing.T) {
	server := newHTTPServer(":0", []readinessCheck{
		okCheck("postgres"), okCheck("schema"), okCheck("encryption_key"),
	})

	rec := do(t, server, "/health/ready")
	if rec.Code != http.StatusOK {
		t.Errorf("код %d, ожидался 200", rec.Code)
	}
}

func TestReadinessFailsOnFirstFailingCheck(t *testing.T) {
	server := newHTTPServer(":0", []readinessCheck{
		okCheck("postgres"),
		failingCheck("schema", errors.New("схема версии 0, бинарю нужна не ниже 1")),
		okCheck("encryption_key"),
	})

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

	server := newHTTPServer(":0", []readinessCheck{
		failingCheck("postgres", errors.New(detail)),
	})

	body := do(t, server, "/health/ready").Body.String()
	for _, leaked := range []string{"db.internal", "hunter2", "password"} {
		if strings.Contains(body, leaked) {
			t.Errorf("тело %q раскрывает %q", body, leaked)
		}
	}
}

// TestReadinessStopsAtFirstFailure — проверки идут по возрастанию стоимости, и
// спрашивать схему у недоступной базы незачем.
func TestReadinessStopsAtFirstFailure(t *testing.T) {
	reachedSchema := false

	server := newHTTPServer(":0", []readinessCheck{
		failingCheck("postgres", errors.New("недоступна")),
		{name: "schema", run: func(context.Context) error {
			reachedSchema = true
			return nil
		}},
	})

	do(t, server, "/health/ready")

	if reachedSchema {
		t.Error("проверка схемы выполнена, хотя база уже признана недоступной")
	}
}
