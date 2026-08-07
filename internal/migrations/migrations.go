// Пакет migrations содержит versioned SQL-миграции схемы и отдаёт мигратор,
// привязанный к ним. Схема меняется только versioned SQL; GORM AutoMigrate не
// используется (BACKEND_DOMAIN_AGREEMENTS.md §11).
package migrations

import (
	"database/sql"
	"embed"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// files держит встроенные (embed) *.sql-миграции, чтобы бинарь был самодостаточным
// и не читал миграции с файловой системы в рантайме.
//
//go:embed *.sql
var files embed.FS

// New строит Migrate над встроенными миграциями, привязанный к существующему
// *sql.DB. DB должен быть открыт драйвером lib/pq "postgres": postgres-драйвер
// golang-migrate выполняет каждую миграцию одним multi-statement Exec (одна неявная
// транзакция) и берёт PostgreSQL advisory lock на время миграции, что удовлетворяет
// §11 («миграции запускаются под advisory lock до rollout приложения»).
//
// Владелец db — вызывающий, он же отвечает за его закрытие.
func New(db *sql.DB) (*migrate.Migrate, error) {
	src, err := iofs.New(files, ".")
	if err != nil {
		return nil, fmt.Errorf("migrations: open embedded source: %w", err)
	}

	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		return nil, fmt.Errorf("migrations: postgres driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "postgres", driver)
	if err != nil {
		return nil, fmt.Errorf("migrations: init migrator: %w", err)
	}
	return m, nil
}
