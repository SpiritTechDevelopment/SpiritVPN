package database

import (
	"database/sql"
	"fmt"
	"log"

	"github.com/RomanRyabinkin/SpiritVPN/pkg/config"
	_ "github.com/lib/pq"
)

// DB представляет подключение к базе данных PostgreSQL.
// Инкапсулирует *sql.DB и предоставляет методы для работы с базой.
type DB struct {
	conn *sql.DB
}

// Connect устанавливает подключение к базе данных PostgreSQL.
// Проверяет доступность базы через ping и возвращает готовое к использованию подключение.
//
// Параметры:
//   - cfg: конфигурация с параметрами подключения к БД
//
// Возвращает:
//   - *DB: инициализированное подключение к базе данных
//   - error: ошибка подключения или nil при успехе
func Connect(cfg *config.Config) (*DB, error) {
	dsn := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
		cfg.Database.Host,
		cfg.Database.Port,
		cfg.Database.User,
		cfg.Database.Password,
		cfg.Database.Name,
	)

	conn, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	if err := conn.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	log.Println("Successfully connected to database")

	return &DB{conn: conn}, nil
}

// Close корректно закрывает подключение к базе данных.
// Должен вызываться при завершении работы приложения.
//
// Возвращает:
//   - error: ошибка закрытия или nil при успехе
func (db *DB) Close() error {
	if db.conn != nil {
		return db.conn.Close()
	}
	return nil
}

// Migrate выполняет миграции схемы базы данных.
// Создает необходимые таблицы, индексы и связи если они еще не существуют.
// Безопасно для повторного вызова (использует IF NOT EXISTS).
//
// Параметры:
//   - db: подключение к базе данных
//
// Возвращает:
//   - error: ошибка миграции или nil при успехе
//
// TODO: Использовать migrate библиотеку или GORM AutoMigrate для версионирования миграций
func Migrate(db *DB) error {
	// TODO: Использовать migrate библиотеку или GORM AutoMigrate
	log.Println("Running database migrations...")

	// Создание таблиц
	queries := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id SERIAL PRIMARY KEY,
			telegram_id BIGINT UNIQUE NOT NULL,
			username VARCHAR(255),
			email VARCHAR(255),
			created_at TIMESTAMP DEFAULT NOW(),
			updated_at TIMESTAMP DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS subscriptions (
			id SERIAL PRIMARY KEY,
			user_id INTEGER REFERENCES users(id),
			plan_type VARCHAR(50) NOT NULL,
			start_date TIMESTAMP NOT NULL,
			end_date TIMESTAMP NOT NULL,
			is_active BOOLEAN DEFAULT true,
			auto_renew BOOLEAN DEFAULT false,
			created_at TIMESTAMP DEFAULT NOW()
		)`,
		// TODO: Добавить остальные таблицы
	}

	for _, query := range queries {
		if _, err := db.conn.Exec(query); err != nil {
			return fmt.Errorf("migration failed: %w", err)
		}
	}

	log.Println("Migrations completed successfully")
	return nil
}
