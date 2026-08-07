// Команда migrate накатывает или откатывает versioned SQL-миграции схемы.
// Это шаг деплоя, выполняемый до rollout приложения, а не то, что делает само
// приложение при старте (BACKEND_DOMAIN_AGREEMENTS.md §11).
//
// Использование:
//
//	migrate up              накатить все ожидающие миграции
//	migrate down            откатить ровно одну миграцию
//	migrate down-all        откатить все миграции (только dev)
//	migrate version         показать текущую версию схемы
//	migrate force <v>       выставить версию <v> без выполнения (починить dirty-состояние)
//
// Параметры подключения берутся из DATABASE_URL либо из DB_HOST/DB_PORT/DB_USER/
// DB_PASSWORD/DB_NAME/DB_SSLMODE (те же имена, что в pkg/config).
package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/lib/pq"

	"github.com/RomanRyabinkin/SpiritVPN/internal/migrations"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(cmd string, args []string) error {
	db, err := sql.Open("postgres", dsn())
	if err != nil {
		return fmt.Errorf("открыть базу: %w", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		return fmt.Errorf("подключиться к базе: %w", err)
	}

	m, err := migrations.New(db)
	if err != nil {
		return err
	}

	switch cmd {
	case "up":
		return report("up", m.Up())
	case "down":
		return report("down", m.Steps(-1))
	case "down-all":
		return report("down-all", m.Down())
	case "version":
		return printVersion(m)
	case "force":
		if len(args) != 1 {
			return errors.New("force требует ровно один аргумент-версию")
		}
		v, convErr := strconv.Atoi(args[0])
		if convErr != nil {
			return fmt.Errorf("неверная версия %q: %w", args[0], convErr)
		}
		if forceErr := m.Force(v); forceErr != nil {
			return forceErr
		}
		fmt.Printf("версия принудительно выставлена в %d\n", v)
		return nil
	default:
		usage()
		return fmt.Errorf("неизвестная команда %q", cmd)
	}
}

// report трактует migrate.ErrNoChange (нечего делать) как успех.
func report(action string, err error) error {
	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("%s: %w", action, err)
	}
	if errors.Is(err, migrate.ErrNoChange) {
		fmt.Printf("%s: изменений нет\n", action)
		return nil
	}
	fmt.Printf("%s: ok\n", action)
	return nil
}

func printVersion(m *migrate.Migrate) error {
	v, dirty, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		fmt.Println("версия: нет (миграции не применялись)")
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Printf("версия: %d (dirty=%t)\n", v, dirty)
	return nil
}

// dsn возвращает строку подключения lib/pq. DATABASE_URL приоритетнее, если задан;
// иначе строка собирается из переменных DB_*.
func dsn() string {
	if u := os.Getenv("DATABASE_URL"); u != "" {
		return u
	}
	host := env("DB_HOST", "localhost")
	port := env("DB_PORT", "5432")
	user := env("DB_USER", "spiritdb")
	pass := os.Getenv("DB_PASSWORD")
	name := env("DB_NAME", "spiritdb")
	sslmode := env("DB_SSLMODE", "disable")

	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(user, pass),
		Host:   host + ":" + port,
		Path:   "/" + name,
	}
	q := u.Query()
	q.Set("sslmode", sslmode)
	u.RawQuery = q.Encode()
	return u.String()
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func usage() {
	fmt.Fprintln(os.Stderr, "использование: migrate {up|down|down-all|version|force <v>}")
}
