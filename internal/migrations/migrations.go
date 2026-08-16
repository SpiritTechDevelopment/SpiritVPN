// Пакет migrations содержит versioned SQL-миграции схемы и отдаёт мигратор,
// привязанный к ним. Схема меняется только versioned SQL; GORM AutoMigrate не
// используется.
package migrations

import (
	"database/sql"
	"embed"
	"fmt"
	"strconv"
	"strings"

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
// транзакция) и берёт PostgreSQL advisory lock на время миграции. Миграции
// запускаются под этим lock до rollout приложения.
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

// Latest возвращает версию последней встроенной миграции.
//
// Нужна readiness, которая пропускает только совместимую схему. Процесс, чья
// схема старше его собственных миграций, остаётся not ready: иначе он
// начнёт принимать команды против схемы, в которой нет нужных ему таблиц и
// колонок, и упадёт уже на первом запросе вместо того, чтобы просто не попасть в
// балансировку.
//
// Версия берётся из имён файлов, а не задаётся константой: константу забудут
// поднять вместе с новой миграцией, и readiness начнёт врать.
func Latest() (uint, error) {
	entries, err := files.ReadDir(".")
	if err != nil {
		return 0, fmt.Errorf("migrations: чтение встроенных миграций: %w", err)
	}

	var latest uint
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}

		digits, _, found := strings.Cut(name, "_")
		if !found {
			return 0, fmt.Errorf("migrations: имя %q не начинается с версии", name)
		}

		version, err := strconv.ParseUint(digits, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("migrations: версия в имени %q не разбирается: %w", name, err)
		}
		if uint(version) > latest {
			latest = uint(version)
		}
	}

	if latest == 0 {
		return 0, fmt.Errorf("migrations: не найдено ни одной up-миграции")
	}
	return latest, nil
}
