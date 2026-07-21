package database

import (
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// setupTestDB создает тестовую БД с sqlmock для unit-тестов
func setupTestDB(t *testing.T) (*gorm.DB, sqlmock.Sqlmock, func()) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}

	dialector := postgres.New(postgres.Config{
		Conn:       db,
		DriverName: "postgres",
	})

	gormDB, err := gorm.Open(dialector, &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open gorm db: %v", err)
	}

	cleanup := func() {
		_ = db.Close()
	}

	return gormDB, mock, cleanup
}

// TestTrafficStatsRepository_Create проверяет создание записи статистики
func TestTrafficStatsRepository_Create(t *testing.T) {
	gormDB, mock, cleanup := setupTestDB(t)
	defer cleanup()

	repo := &TrafficStatsRepository{db: gormDB}

	stat := &TrafficStat{
		UserID:        1,
		ConfigID:      1,
		BytesSent:     1024,
		BytesReceived: 2048,
		Date:          time.Now().Truncate(24 * time.Hour),
	}

	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "traffic_stats"`).
		WithArgs(
			stat.UserID,
			stat.ConfigID,
			stat.BytesSent,
			stat.BytesReceived,
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	mock.ExpectCommit()

	err := repo.Create(stat)
	assert.NoError(t, err)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestTrafficStatsRepository_GetByUserID проверяет получение статистики пользователя
func TestTrafficStatsRepository_GetByUserID(t *testing.T) {
	gormDB, mock, cleanup := setupTestDB(t)
	defer cleanup()

	repo := &TrafficStatsRepository{db: gormDB}

	userID := uint(1)
	testDate := time.Now().Truncate(24 * time.Hour)

	rows := sqlmock.NewRows([]string{"id", "user_id", "config_id", "bytes_sent", "bytes_received", "date", "created_at"}).
		AddRow(1, userID, 1, 1024, 2048, testDate, time.Now()).
		AddRow(2, userID, 2, 512, 1024, testDate.AddDate(0, 0, -1), time.Now())

	mock.ExpectQuery(`SELECT \* FROM "traffic_stats"`).
		WithArgs(userID).
		WillReturnRows(rows)

	stats, err := repo.GetByUserID(userID)
	assert.NoError(t, err)
	assert.Len(t, stats, 2)
	assert.Equal(t, userID, stats[0].UserID)
	assert.Equal(t, int64(1024), stats[0].BytesSent)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestTrafficStatsRepository_UpsertDailyStats_Create проверяет создание новой записи при upsert
func TestTrafficStatsRepository_UpsertDailyStats_Create(t *testing.T) {
	gormDB, mock, cleanup := setupTestDB(t)
	defer cleanup()

	repo := &TrafficStatsRepository{db: gormDB}

	stat := &TrafficStat{
		UserID:        1,
		ConfigID:      1,
		BytesSent:     1024,
		BytesReceived: 2048,
		Date:          time.Now().Truncate(24 * time.Hour),
	}

	mock.ExpectQuery(`SELECT \* FROM "traffic_stats"`).
		WithArgs(stat.UserID, stat.ConfigID, sqlmock.AnyArg(), 1).
		WillReturnError(gorm.ErrRecordNotFound)

	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "traffic_stats"`).
		WithArgs(
			stat.UserID,
			stat.ConfigID,
			stat.BytesSent,
			stat.BytesReceived,
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	mock.ExpectCommit()

	err := repo.UpsertDailyStats(stat)
	assert.NoError(t, err)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestTrafficStatsRepository_UpsertDailyStats_Update проверяет обновление существующей записи при upsert
func TestTrafficStatsRepository_UpsertDailyStats_Update(t *testing.T) {
	gormDB, mock, cleanup := setupTestDB(t)
	defer cleanup()

	repo := &TrafficStatsRepository{db: gormDB}

	existingStat := &TrafficStat{
		ID:            1,
		UserID:        1,
		ConfigID:      1,
		BytesSent:     500,
		BytesReceived: 1000,
		Date:          time.Now().Truncate(24 * time.Hour),
	}

	newStat := &TrafficStat{
		UserID:        1,
		ConfigID:      1,
		BytesSent:     1024,
		BytesReceived: 2048,
		Date:          existingStat.Date,
	}

	rows := sqlmock.NewRows([]string{"id", "user_id", "config_id", "bytes_sent", "bytes_received", "date", "created_at"}).
		AddRow(existingStat.ID, existingStat.UserID, existingStat.ConfigID, existingStat.BytesSent, existingStat.BytesReceived, existingStat.Date, time.Now())

	mock.ExpectQuery(`SELECT \* FROM "traffic_stats"`).
		WithArgs(newStat.UserID, newStat.ConfigID, sqlmock.AnyArg(), 1).
		WillReturnRows(rows)

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "traffic_stats"`).
		WithArgs(
			existingStat.UserID,
			existingStat.ConfigID,
			existingStat.BytesSent+newStat.BytesSent,
			existingStat.BytesReceived+newStat.BytesReceived,
			sqlmock.AnyArg(),                                 // date
			sqlmock.AnyArg(),                                 // created_at
			existingStat.ID,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	err := repo.UpsertDailyStats(newStat)
	assert.NoError(t, err)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestSubscriptionRepository_GetAllActive проверяет получение всех активных подписок
func TestSubscriptionRepository_GetAllActive(t *testing.T) {
	gormDB, mock, cleanup := setupTestDB(t)
	defer cleanup()

	repo := &SubscriptionRepository{db: gormDB}

	now := time.Now()
	futureDate := now.AddDate(0, 0, 30)

	rows := sqlmock.NewRows([]string{"id", "user_id", "plan_id", "is_active", "start_date", "end_date", "created_at", "updated_at"}).
		AddRow(1, 1, 1, true, now, futureDate, now, now).
		AddRow(2, 2, 1, true, now, futureDate, now, now)

	mock.ExpectQuery(`SELECT \* FROM "subscriptions"`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(rows)

	subscriptions, err := repo.GetAllActive()
	assert.NoError(t, err)
	assert.Len(t, subscriptions, 2)
	assert.True(t, subscriptions[0].IsActive)
	assert.True(t, subscriptions[1].IsActive)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestVPNConfigRepository_GetBySubscriptionID проверяет получение конфигураций по ID подписки
func TestVPNConfigRepository_GetBySubscriptionID(t *testing.T) {
	gormDB, mock, cleanup := setupTestDB(t)
	defer cleanup()

	repo := &VPNConfigRepository{db: gormDB}

	subscriptionID := uint(1)

	// Mock для основного запроса конфигураций
	configRows := sqlmock.NewRows([]string{"id", "subscription_id", "user_id", "server_id", "uuid", "is_active", "created_at", "updated_at"}).
		AddRow(1, subscriptionID, 1, 1, "test-uuid-1", true, time.Now(), time.Now()).
		AddRow(2, subscriptionID, 1, 2, "test-uuid-2", true, time.Now(), time.Now())

	mock.ExpectQuery(`SELECT \* FROM "vpn_configs"`).
		WithArgs(subscriptionID).
		WillReturnRows(configRows)

	// Mock для Preload("Server").GORM загружает все одним запросом
	serverRows := sqlmock.NewRows([]string{"id", "location", "ip_address", "port", "is_active", "current_users", "max_users", "load_percent", "created_at", "updated_at"}).
		AddRow(1, "US", "1.2.3.4", 443, true, 10, 100, 10.0, time.Now(), time.Now()).
		AddRow(2, "EU", "5.6.7.8", 443, true, 5, 100, 5.0, time.Now(), time.Now())

	mock.ExpectQuery(`SELECT \* FROM "vpn_servers"`).
		WithArgs(1, 2).
		WillReturnRows(serverRows)

	configs, err := repo.GetBySubscriptionID(subscriptionID)
	assert.NoError(t, err)
	assert.Len(t, configs, 2)
	assert.Equal(t, subscriptionID, configs[0].SubscriptionID)
	assert.Equal(t, "test-uuid-1", configs[0].UUID)
	assert.NoError(t, mock.ExpectationsWereMet())
}
