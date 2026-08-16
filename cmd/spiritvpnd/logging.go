package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/tracelog"

	"github.com/RomanRyabinkin/SpiritVPN/internal/grpcsvc"
)

// newLogger собирает структурный логгер процесса.
//
// JSON и stderr. stdout остаётся свободным под данные, его читают пайплайны;
// логи из файла в контейнере собирает платформа.
func newLogger(level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// pgxLogger переносит записи pgx в slog.
//
// У метода Log есть ctx, поэтому запрос к базе логируется с тем же request_id,
// что и породивший его RPC. Без этого «медленный запрос» и «медленный вызов»
// остаются двумя несвязанными наблюдениями.
type pgxLogger struct {
	logger *slog.Logger
}

// argsKey — ключ, под которым pgx кладёт аргументы запроса.
const argsKey = "args"

func (l pgxLogger) Log(ctx context.Context, level tracelog.LogLevel, msg string, data map[string]any) {
	attrs := make([]slog.Attr, 0, len(data)+1)
	attrs = append(attrs, slog.String("request_id", grpcsvc.RequestIDFromContext(ctx)))

	for key, value := range data {
		// Аргументы запроса не логируются никогда: среди них customer_id,
		// допустимый только в audit records, и зашифрованный credential.
		// Отбрасываются здесь, а не уровнем логирования, потому что debug на
		// проде включают когда что-то сломалось — худший момент для утечки.
		if key == argsKey {
			continue
		}
		attrs = append(attrs, slog.Any(key, value))
	}

	l.logger.LogAttrs(ctx, pgxLevel(level), "postgres: "+msg, attrs...)
}

// pgxLevel переводит уровни pgx в уровни slog.
func pgxLevel(level tracelog.LogLevel) slog.Level {
	switch level {
	case tracelog.LogLevelTrace, tracelog.LogLevelDebug:
		return slog.LevelDebug
	case tracelog.LogLevelInfo:
		return slog.LevelInfo
	case tracelog.LogLevelWarn:
		return slog.LevelWarn
	default:
		return slog.LevelError
	}
}

// tracerLevel выбирает, насколько подробно pgx рассказывает о запросах.
//
// На debug логируется каждый запрос с длительностью — ради этого debug и
// включают. На остальных уровнях только ошибки: успешный запрос в короткой
// транзакции ничего не сообщает, а объём такого лога пропорционален трафику.
func tracerLevel(level slog.Level) tracelog.LogLevel {
	if level <= slog.LevelDebug {
		return tracelog.LogLevelDebug
	}
	return tracelog.LogLevelError
}

// traceLog собирает трейсер запросов для пула.
func traceLog(logger *slog.Logger, level slog.Level) *tracelog.TraceLog {
	return &tracelog.TraceLog{
		Logger:   pgxLogger{logger: logger},
		LogLevel: tracerLevel(level),
	}
}
