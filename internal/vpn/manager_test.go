package vpn_test

import (
	"context"
	"testing"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/database"
	"github.com/RomanRyabinkin/SpiritVPN/internal/vpn"
	"github.com/RomanRyabinkin/SpiritVPN/pkg/config"
	"github.com/RomanRyabinkin/SpiritVPN/pkg/logger"
	"github.com/stretchr/testify/suite"
	"fmt"
)

type ManagerTestSuite struct {
	suite.Suite
	db      *database.DB
	manager *vpn.Manager
	user    *database.User
	plan    *database.SubscriptionPlan
	server  *database.VPNServer
}

func (s *ManagerTestSuite) SetupSuite() {
	_ = logger.Setup(&logger.Config{Enabled: false}) 

	cfg := &config.Config{
		Database: config.DatabaseConfig{
			Host: "localhost", Port: 5432, User: "spiritdb", Password: "your_secure_password", Name: "spiritdb",
		},
	}
	db, err := database.Connect(cfg)
	s.Require().NoError(err, "Ожидается успешное подключение к БД")
	s.db = db

	s.manager = vpn.NewManager(db, nil)
}

func (s *ManagerTestSuite) SetupTest() {
	gormDB := s.db.GetDB()

	s.user = &database.User{TelegramID: time.Now().UnixNano(), Username: "test_integration"}
	s.Require().NoError(gormDB.Create(s.user).Error)

	s.plan = &database.SubscriptionPlan{Code: "test_plan", DurationDays: 30, Price: 100, IsActive: true, Name: "Test"}
	gormDB.Where("code = ?", "test_plan").FirstOrCreate(s.plan)

	s.server = &database.VPNServer{
		Name: fmt.Sprintf("TestServer-%d", time.Now().UnixNano()), Host: "1.1.1.1", Port: 443, 
		PublicKey: "key", IsActive: true, MaxUsers: 10, CurrentUsers: 0,
	}
	s.Require().NoError(gormDB.Create(s.server).Error)
}

func (s *ManagerTestSuite) TearDownTest() {
	gormDB := s.db.GetDB()
	gormDB.Exec("DELETE FROM vpn_configs WHERE user_id = ?", s.user.ID)
	gormDB.Exec("DELETE FROM subscriptions WHERE user_id = ?", s.user.ID)
	gormDB.Exec("DELETE FROM vpn_servers WHERE id = ?", s.server.ID)
	gormDB.Exec("DELETE FROM users WHERE id = ?", s.user.ID)
}

// Проверка выдачи ВПН новому юзеру
func (s *ManagerTestSuite) TestGrantAccess_NewSubscription() {
	ctx := context.Background()

	err := s.manager.GrantAccess(ctx, s.user.ID, s.plan.Code)
	s.Require().NoError(err, "GrantAccess не должен возвращать ошибку")

	gormDB := s.db.GetDB()

	var sub database.Subscription
	err = gormDB.Where("user_id = ?", s.user.ID).First(&sub).Error
	s.Require().NoError(err)
	s.True(sub.IsActive)
	
	expectedEndDate := time.Now().AddDate(0, 0, s.plan.DurationDays)
	s.WithinDuration(expectedEndDate, sub.EndDate, 5*time.Second)

	var config database.VPNConfig
	err = gormDB.Where("user_id = ?", s.user.ID).First(&config).Error
	s.Require().NoError(err)
	s.NotEmpty(config.UUID, "UUID должен быть сгенерирован")
	s.Equal(s.server.ID, config.ServerID)

	var updatedServer database.VPNServer
	gormDB.First(&updatedServer, s.server.ID)
	s.Equal(1, updatedServer.CurrentUsers)
}

// Проверка продление существующего доступа
func (s *ManagerTestSuite) TestGrantAccess_ExtendSubscription() {
	ctx := context.Background()

	s.Require().NoError(s.manager.GrantAccess(ctx, s.user.ID, s.plan.Code))

	var initialSub database.Subscription
	s.db.GetDB().Where("user_id = ?", s.user.ID).First(&initialSub)

	err := s.manager.GrantAccess(ctx, s.user.ID, s.plan.Code)
	s.Require().NoError(err)

	var extendedSub database.Subscription
	s.db.GetDB().Where("user_id = ?", s.user.ID).First(&extendedSub)
	
	expectedExtendedDate := initialSub.EndDate.AddDate(0, 0, s.plan.DurationDays)
	s.WithinDuration(expectedExtendedDate, extendedSub.EndDate, 2*time.Second)

	var subCount int64
	s.db.GetDB().Model(&database.Subscription{}).Where("user_id = ?", s.user.ID).Count(&subCount)
	s.Equal(int64(1), subCount, "Должна остаться только 1 подписка")
}

func TestManagerSuite(t *testing.T) {
	suite.Run(t, new(ManagerTestSuite))
}