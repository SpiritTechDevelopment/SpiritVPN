package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RomanRyabinkin/SpiritVPN/internal/config"
)

// connectTimeout ограничивает первую проверку связи с базой.
//
// Без него недоступная база превращает старт в бесконечное ожидание: процесс не
// отвечает ни на readiness, ни на SIGTERM, и под висит до конца grace period
// вместо того, чтобы упасть и перезапуститься.
const connectTimeout = 10 * time.Second

// newPool открывает пул соединений с PostgreSQL.
func newPool(ctx context.Context, cfg config.Config, logger *slog.Logger) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.Postgres.URL)
	if err != nil {
		// Текст ошибки pgx содержит разобранный DSN вместе с паролем и наружу не
		// идёт. Имени переменной достаточно, чтобы понять, что чинить.
		return nil, fmt.Errorf("%s не разбирается как DSN PostgreSQL", config.EnvDatabaseURL)
	}

	poolCfg.MaxConns = cfg.Postgres.MaxConns
	poolCfg.ConnConfig.Tracer = traceLog(logger, cfg.Log.Level)

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("пул PostgreSQL: %w", err)
	}

	// pgxpool.New соединение не открывает. Без явной проверки процесс объявил бы
	// себя поднявшимся, а упала бы первая же команда; readiness поймает это и
	// позже, но узнавать о неверном DSN на старте дешевле.
	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("подключение к PostgreSQL: %w", err)
	}

	return pool, nil
}
