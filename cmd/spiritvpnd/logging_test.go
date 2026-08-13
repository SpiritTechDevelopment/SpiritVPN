package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/tracelog"
)

func logPgxRecord(t *testing.T, data map[string]any) (map[string]any, string) {
	t.Helper()

	var buf bytes.Buffer
	adapter := pgxLogger{
		logger: slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	adapter.Log(context.Background(), tracelog.LogLevelInfo, "Query", data)

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("запись не разбирается как JSON: %v (%s)", err, buf.String())
	}
	return record, buf.String()
}

// TestPgxLoggerDropsQueryArgs — customer_id допустим только в audit records.
// pgx кладёт аргументы запроса в data, и среди них едут и customer_id, и
// зашифрованный credential.
func TestPgxLoggerDropsQueryArgs(t *testing.T) {
	record, raw := logPgxRecord(t, map[string]any{
		"sql":      "SELECT * FROM customer_entitlements WHERE customer_id = $1",
		"args":     []any{"cust-секретный", []byte{0xDE, 0xAD}},
		"time":     "1.2ms",
		"rowCount": 1,
	})

	if _, present := record[argsKey]; present {
		t.Errorf("аргументы запроса попали в лог: %s", raw)
	}
	if strings.Contains(raw, "cust-секретный") {
		t.Errorf("customer_id попал в лог: %s", raw)
	}

	// Полезное остаётся: без sql и длительности запись бесполезна.
	if got, _ := record["sql"].(string); !strings.Contains(got, "customer_entitlements") {
		t.Errorf("sql пропал из записи: %s", raw)
	}
	if _, present := record["rowCount"]; !present {
		t.Errorf("rowCount пропал из записи: %s", raw)
	}
}

// TestPgxLoggerCarriesRequestID — вся ценность адаптера в том, что запрос к базе
// логируется с тем же request_id, что и породивший его RPC.
func TestPgxLoggerCarriesRequestID(t *testing.T) {
	var buf bytes.Buffer
	adapter := pgxLogger{
		logger: slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}

	adapter.Log(context.Background(), tracelog.LogLevelError, "Query", nil)

	if !strings.Contains(buf.String(), `"request_id"`) {
		t.Errorf("в записи нет request_id: %s", buf.String())
	}
}

func TestPgxLevelMapping(t *testing.T) {
	tests := []struct {
		in   tracelog.LogLevel
		want slog.Level
	}{
		{tracelog.LogLevelTrace, slog.LevelDebug},
		{tracelog.LogLevelDebug, slog.LevelDebug},
		{tracelog.LogLevelInfo, slog.LevelInfo},
		{tracelog.LogLevelWarn, slog.LevelWarn},
		{tracelog.LogLevelError, slog.LevelError},
		{tracelog.LogLevelNone, slog.LevelError},
	}

	for _, tc := range tests {
		if got := pgxLevel(tc.in); got != tc.want {
			t.Errorf("уровень pgx %v дал %v, ожидался %v", tc.in, got, tc.want)
		}
	}
}

// TestTracerLevelIsQuietOutsideDebug — успешный запрос в короткой транзакции
// сам по себе ничего не сообщает, а объём такого лога пропорционален
// трафику.
func TestTracerLevelIsQuietOutsideDebug(t *testing.T) {
	if got := tracerLevel(slog.LevelDebug); got != tracelog.LogLevelDebug {
		t.Errorf("на debug уровень трейсера %v, ожидался Debug", got)
	}
	for _, level := range []slog.Level{slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
		if got := tracerLevel(level); got != tracelog.LogLevelError {
			t.Errorf("на %v уровень трейсера %v, ожидался Error", level, got)
		}
	}
}
