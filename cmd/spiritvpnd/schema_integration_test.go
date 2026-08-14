package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
)

// Интеграционные тесты проверки готовности. Смысл schemaCheck целиком в запросе к
// базе: он читает schema_migrations и решает, обслуживать ли вызовы вообще.
// Юнит-тест здесь проверял бы копию запроса, а не запрос.
//
// Гейт тот же, что у адаптера postgres: SPIRITVPN_INTEGRATION_TESTS вместе с
// DATABASE_URL.
//
// Собственная таблица schema_migrations в отдельной схеме, а не настоящая.
// Тесты двигают версию и поднимают флаг dirty, и делать это с рабочей таблицей
// нельзя: по ней идут миграции пакета internal/postgres, а его тесты и эти
// исполняются параллельными процессами одного `go test ./...`.
const schemaCheckSchema = "spiritvpnd_schema_check_test"

// newSchemaPool поднимает пул, чей search_path указывает на отдельную схему.
//
// Именно так «SELECT ... FROM schema_migrations» внутри schemaCheck попадает в
// таблицу теста, хотя имя в запросе захардкожено.
func newSchemaPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	if os.Getenv("SPIRITVPN_INTEGRATION_TESTS") == "" {
		t.Skip("интеграционный тест: задайте SPIRITVPN_INTEGRATION_TESTS и DATABASE_URL")
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Fatal("SPIRITVPN_INTEGRATION_TESTS задан, но DATABASE_URL пуст")
	}

	ctx := context.Background()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pgxpool.ParseConfig: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schemaCheckSchema

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgxpool.NewWithConfig: %v", err)
	}
	t.Cleanup(pool.Close)

	// Схема пересоздаётся на каждый тест: предыдущий мог оставить в ней и другую
	// версию, и поднятый dirty.
	for _, statement := range []string{
		"DROP SCHEMA IF EXISTS " + schemaCheckSchema + " CASCADE",
		"CREATE SCHEMA " + schemaCheckSchema,
		// Раскладка та же, что создаёт golang-migrate.
		"CREATE TABLE " + schemaCheckSchema + `.schema_migrations (
		     version bigint NOT NULL PRIMARY KEY,
		     dirty   boolean NOT NULL
		 )`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("подготовка схемы (%s): %v", statement, err)
		}
	}

	return pool
}

// setSchemaVersion кладёт единственную строку версии.
func setSchemaVersion(t *testing.T, pool *pgxpool.Pool, version int64, dirty bool) {
	t.Helper()

	ctx := context.Background()
	if _, err := pool.Exec(ctx, "DELETE FROM schema_migrations"); err != nil {
		t.Fatalf("очистка версии: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO schema_migrations (version, dirty) VALUES ($1, $2)", version, dirty); err != nil {
		t.Fatalf("запись версии %d: %v", version, err)
	}
}

// Схема ровно той версии, что встроена в бинарь: обычное состояние после деплоя.
func TestIntegrationSchemaCheckPassesOnExactVersion(t *testing.T) {
	pool := newSchemaPool(t)
	setSchemaVersion(t, pool, 5, false)

	if err := schemaCheck(pool, 5)(context.Background()); err != nil {
		t.Errorf("schemaCheck вернул %v на совпадающей версии", err)
	}
}

// Схема новее бинаря — штатное состояние rollout: миграции накатываются до
// подмены процесса, и старый бинарь какое-то время живёт с новой схемой. Откажи
// проверка здесь, обслуживание встало бы ровно в момент выкатки.
func TestIntegrationSchemaCheckPassesOnNewerSchema(t *testing.T) {
	pool := newSchemaPool(t)
	setSchemaVersion(t, pool, 9, false)

	if err := schemaCheck(pool, 5)(context.Background()); err != nil {
		t.Errorf("schemaCheck вернул %v на схеме новее бинаря", err)
	}
}

// Схема старее бинаря означает пропущенный шаг деплоя: образ подменили, миграции
// не накатили. Процесс обязан остаться not ready, а не принимать команды на
// схеме без нужных ему колонок.
func TestIntegrationSchemaCheckRejectsOlderSchema(t *testing.T) {
	pool := newSchemaPool(t)
	setSchemaVersion(t, pool, 4, false)

	err := schemaCheck(pool, 7)(context.Background())
	if err == nil {
		t.Fatal("schemaCheck принял схему старее бинаря")
	}
	// Обе версии обязаны быть в сообщении: по нему оператор понимает, чего не
	// хватает, не заходя в базу.
	for _, want := range []string{"4", "7"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("сообщение %q не называет версию %s", err, want)
		}
	}
}

// dirty означает прерванную миграцию: схема в неизвестном состоянии, и принимать
// на ней команды нельзя даже при подходящем номере версии.
func TestIntegrationSchemaCheckRejectsDirtySchema(t *testing.T) {
	pool := newSchemaPool(t)
	setSchemaVersion(t, pool, 9, true)

	err := schemaCheck(pool, 5)(context.Background())
	if err == nil {
		t.Fatal("schemaCheck принял dirty-схему: номер версии сам по себе её не оправдывает")
	}
	if !strings.Contains(err.Error(), "dirty") {
		t.Errorf("сообщение %q не называет причину", err)
	}
}

// Пустая или отсутствующая таблица версий — это тоже отказ, а не «версия ноль».
// Так выглядит база, на которую миграции не накатывали ни разу.
func TestIntegrationSchemaCheckRejectsMissingVersionRow(t *testing.T) {
	pool := newSchemaPool(t)

	if err := schemaCheck(pool, 1)(context.Background()); err == nil {
		t.Error("schemaCheck принял базу без единой накатанной миграции")
	}

	if _, err := pool.Exec(context.Background(), "DROP TABLE schema_migrations"); err != nil {
		t.Fatalf("удаление таблицы: %v", err)
	}
	if err := schemaCheck(pool, 1)(context.Background()); err == nil {
		t.Error("schemaCheck принял базу без таблицы версий")
	}
}

// testCipher собирает шифр на детерминированном ключе.
func testReadinessCipher(t *testing.T) *crypto.Cipher {
	t.Helper()

	key, err := crypto.NewKey("test-key", make([]byte, crypto.KeySize))
	if err != nil {
		t.Fatalf("crypto.NewKey: %v", err)
	}
	cipher, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatalf("crypto.NewCipher: %v", err)
	}
	return cipher
}

// Набор проверок готовности идёт целиком до ответа /health/ready.
//
// Проверяется и порядок: имя провалившейся проверки — единственное, что уходит
// наружу, и по нему оператор отличает недоступную базу от несовпавшей схемы.
// Проверка схемы обязана стоять после postgres: спрашивать версию у базы, к
// которой нет соединения, незачем.
func TestIntegrationReadinessNamesFailingCheck(t *testing.T) {
	pool := newSchemaPool(t)
	cipher := testReadinessCipher(t)

	setSchemaVersion(t, pool, 3, false)

	checks := readinessChecks(pool, cipher, 3)
	if len(checks) != 3 {
		t.Fatalf("проверок готовности %d, ожидалось 3", len(checks))
	}
	wantOrder := []string{"postgres", "schema", "encryption_key"}
	for i, want := range wantOrder {
		if checks[i].name != want {
			t.Errorf("проверка %d называется %q, ожидалась %q", i, checks[i].name, want)
		}
	}

	if got := readinessStatus(t, checks); got != http.StatusOK {
		t.Fatalf("код готовности %d на исправном наборе, ожидался 200", got)
	}

	// Схема отстала: наружу обязано уйти имя именно этой проверки.
	setSchemaVersion(t, pool, 2, false)

	recorder := readinessResponse(t, readinessChecks(pool, cipher, 3))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("код %d на отставшей схеме, ожидался 503", recorder.Code)
	}
	if got := recorder.Body.String(); !strings.Contains(got, "not ready: schema") {
		t.Errorf("тело ответа %q не называет провалившуюся проверку", got)
	}
	// Наружу уходит только имя: текст ошибки pgx несёт параметры подключения, а
	// служебный порт доступен всему, что до него дотянулось.
	if got := recorder.Body.String(); strings.Contains(got, "версии") {
		t.Errorf("тело ответа %q раскрывает диагностику проверки", got)
	}
}

func readinessResponse(t *testing.T, checks []readinessCheck) *httptest.ResponseRecorder {
	t.Helper()

	recorder := httptest.NewRecorder()
	handleReady(checks)(recorder, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	return recorder
}

func readinessStatus(t *testing.T, checks []readinessCheck) int {
	t.Helper()
	return readinessResponse(t, checks).Code
}
