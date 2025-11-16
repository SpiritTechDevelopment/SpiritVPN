package database

import (
	"fmt"
	"log"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/pkg/config"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DB представляет подключение к базе данных PostgreSQL через GORM.
// Инкапсулирует *gorm.DB и предоставляет методы для работы с базой.
type DB struct {
	conn *gorm.DB
}

// Connect устанавливает подключение к базе данных PostgreSQL через GORM.
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

	// Настройка логгера GORM
	gormConfig := &gorm.Config{
		Logger: logger.Default.LogMode(logger.Info),
		NowFunc: func() time.Time {
			return time.Now().UTC()
		},
	}

	// Для production режима отключаем подробное логирование
	if cfg.API.Mode == "production" {
		gormConfig.Logger = logger.Default.LogMode(logger.Error)
	}

	conn, err := gorm.Open(postgres.Open(dsn), gormConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Получаем базовое подключение для настройки пула
	sqlDB, err := conn.DB()
	if err != nil {
		return nil, fmt.Errorf("failed to get database instance: %w", err)
	}

	// Настройка пула подключений
	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetMaxOpenConns(100)
	sqlDB.SetConnMaxLifetime(time.Hour)

	// Проверка подключения
	if err := sqlDB.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	log.Println("Successfully connected to database via GORM")

	return &DB{conn: conn}, nil
}

// GetDB возвращает GORM подключение для выполнения запросов.
// Используется репозиториями для доступа к базе данных.
//
// Возвращает:
//   - *gorm.DB: GORM подключение для выполнения SQL запросов
//
// Пример:
//
//	db := database.Connect(cfg)
//	gormDB := db.GetDB()
//	userRepo := repository.NewUserRepository(db)
func (db *DB) GetDB() *gorm.DB {
	return db.conn
}

// Close корректно закрывает подключение к базе данных.
// Должен вызываться при завершении работы приложения для освобождения ресурсов.
// Закрывает все активные соединения в пуле подключений.
//
// Возвращает:
//   - error: ошибка закрытия или nil при успехе
//
// Пример:
//
//	defer db.Close()
func (db *DB) Close() error {
	if db.conn != nil {
		sqlDB, err := db.conn.DB()
		if err != nil {
			return err
		}
		return sqlDB.Close()
	}
	return nil
}

// Migrate выполняет миграции схемы базы данных через GORM AutoMigrate.
// Создает необходимые таблицы, индексы и связи если они еще не существуют.
//
// Функция выполняет следующие действия:
//  1. AutoMigrate для 7 моделей (User, Subscription, VPNConfig, VPNServer, Payment, TrafficStat, SubscriptionPlan)
//  2. Создание дополнительных композитных индексов через createAdditionalIndexes()
//  3. Заполнение базовых данных (seed) через seedDefaultData()
//
// Важно: AutoMigrate НЕ удаляет существующие столбцы для безопасности данных.
// Функция безопасна для повторного вызова - не создаст дубликатов.
//
// Параметры:
//   - db: подключение к базе данных
//
// Возвращает:
//   - error: ошибка миграции или nil при успехе
//
// Пример:
//
//	db, _ := database.Connect(cfg)
//	if err := database.Migrate(db); err != nil {
//	    log.Fatal("Migration failed:", err)
//	}
func Migrate(db *DB) error {
	log.Println("Running database migrations with GORM AutoMigrate...")

	// AutoMigrate создаст таблицы, добавит недостающие столбцы и индексы
	// Не удаляет неиспользуемые столбцы для безопасности данных
	err := db.conn.AutoMigrate(
		&User{},
		&Subscription{},
		&VPNConfig{},
		&VPNServer{},
		&Payment{},
		&TrafficStat{},
		&SubscriptionPlan{},
	)

	if err != nil {
		return fmt.Errorf("migration failed: %w", err)
	}

	// Создание дополнительных индексов
	if err := createAdditionalIndexes(db.conn); err != nil {
		return fmt.Errorf("failed to create indexes: %w", err)
	}

	// Заполнение начальными данными (seed)
	if err := seedDefaultData(db.conn); err != nil {
		return fmt.Errorf("failed to seed data: %w", err)
	}

	log.Println("Migrations completed successfully")
	return nil
}

// createAdditionalIndexes создает дополнительные композитные индексы для оптимизации частых запросов.
// Композитные индексы позволяют ускорить запросы с несколькими условиями.
//
// Создаваемые индексы:
//  1. idx_subscriptions_user_active - поиск активных подписок пользователя (user_id, is_active)
//  2. idx_payments_status_created - фильтрация платежей по статусу с сортировкой (status, created_at DESC)
//  3. idx_servers_active_load - поиск доступных серверов с наименьшей загрузкой (is_active, load_percent)
//
// Использует "CREATE INDEX IF NOT EXISTS" для безопасного повторного вызова.
//
// Параметры:
//   - db: GORM подключение к базе данных
//
// Возвращает:
//   - error: ошибка создания индексов или nil при успехе
func createAdditionalIndexes(db *gorm.DB) error {
	// Composite индекс для быстрого поиска активных подписок пользователя
	if err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_subscriptions_user_active 
		ON subscriptions(user_id, is_active) 
		WHERE is_active = true
	`).Error; err != nil {
		return err
	}

	// Индекс для поиска платежей по статусу
	if err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_payments_status_created 
		ON payments(status, created_at DESC)
	`).Error; err != nil {
		return err
	}

	// Индекс для быстрой проверки доступных серверов
	if err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_servers_active_load 
		ON vpn_servers(is_active, load_percent) 
		WHERE is_active = true
	`).Error; err != nil {
		return err
	}

	return nil
}

// seedDefaultData заполняет базу данных начальными (базовыми) данными.
// Создает стандартные тарифные планы если они еще не созданы.
//
// Тарифные планы по умолчанию:
//  1. Basic - 299₽/мес, 1 устройство, 50 Мбит/с, 30 дней
//  2. Premium - 599₽/мес, 5 устройств, безлимит, 30 дней
//  3. Premium Year - 5990₽/год, 5 устройств, безлимит, 365 дней (экономия 16%)
//
// Функция проверяет наличие планов и создает их только если таблица пуста.
// Безопасна для повторного вызова.
//
// Параметры:
//   - db: GORM подключение к базе данных
//
// Возвращает:
//   - error: ошибка заполнения данными или nil при успехе
func seedDefaultData(db *gorm.DB) error {
	// Проверяем, есть ли уже тарифные планы
	var count int64
	db.Model(&SubscriptionPlan{}).Count(&count)
	if count > 0 {
		return nil // Данные уже есть
	}

	log.Println("Seeding default subscription plans...")

	plans := []SubscriptionPlan{
		{
			Name:         "Basic",
			Code:         "basic",
			DurationDays: 30,
			Price:        299.00,
			Currency:     "RUB",
			MaxDevices:   1,
			MaxSpeed:     50,
			Description:  "Базовый тариф для личного использования",
			Features:     `["1 устройство","50 Мбит/с","Базовая поддержка"]`,
			IsActive:     true,
			DisplayOrder: 1,
		},
		{
			Name:         "Premium",
			Code:         "premium",
			DurationDays: 30,
			Price:        599.00,
			Currency:     "RUB",
			MaxDevices:   5,
			MaxSpeed:     0, // Безлимит
			Description:  "Премиум тариф с максимальной скоростью",
			Features:     `["5 устройств","Безлимитная скорость","Приоритетная поддержка","Все серверы"]`,
			IsActive:     true,
			DisplayOrder: 2,
		},
		{
			Name:         "Premium Year",
			Code:         "premium_year",
			DurationDays: 365,
			Price:        5990.00,
			Currency:     "RUB",
			MaxDevices:   5,
			MaxSpeed:     0,
			Description:  "Премиум тариф на год с выгодной ценой",
			Features:     `["5 устройств","Безлимитная скорость","Приоритетная поддержка","Все серверы","Скидка 16%"]`,
			IsActive:     true,
			DisplayOrder: 3,
		},
	}

	return db.Create(&plans).Error
}
