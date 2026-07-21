package vpn

import (
	"context"
	"fmt"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/database"
	"github.com/RomanRyabinkin/SpiritVPN/pkg/logger"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// Отвечает за оркестрацию VPN-доступов.
// Он связывает базу данных и клиент Xray, гарантируя, что изменения 
// в биллинге синхронизированы с реальными доступами на серверах.
type Manager struct {
	db         *database.DB
	xrayClient *XrayClient
	log        *logrus.Entry
}

// NewManager создает новый экземпляр менеджера VPN.
// Принимает подключение к БД и клиент Xray (может быть nil для тестов).
func NewManager(db *database.DB, xrayClient *XrayClient) *Manager {
	return &Manager{
		db:         db,
		xrayClient: xrayClient,
		log:        logger.GetLogger("vpn.manager"),
	}
}

// Выдает новый или продлевает существующий доступ к VPN для пользователя.
// Выполняется внутри транзакции БД.
// 
// Алгоритм:
// 1. Ищет активную подписку. Если есть — продлевает срок (поле EndDate).
// 2. Если нет — создает новую подписку, находит наименее загруженный сервер.
// 3. Генерирует уникальный VLESS UUID, сохраняет конфиг.
// 4. Отправляет команду по gRPC в ядро Xray.
func (m *Manager) GrantAccess(ctx context.Context, userID uint, planCode string) error {
	m.log.WithField("user_id", userID).Info("Starting GrantAccess process")

	planRepo := database.NewSubscriptionPlanRepository(m.db)
	plan, err := planRepo.GetByCode(planCode)
	if err != nil {
		return fmt.Errorf("failed to get plan: %w", err)
	}

	return m.db.GetDB().Transaction(func(tx *gorm.DB) error {
		var activeSub database.Subscription
		err := tx.Where("user_id = ? AND is_active = ?", userID, true).First(&activeSub).Error

		switch err {
		case nil:
			activeSub.EndDate = activeSub.EndDate.AddDate(0, 0, plan.DurationDays)
			tx.Save(&activeSub)
			m.log.WithField("user_id", userID).Infof("Extended subscription until %s", activeSub.EndDate)
		case gorm.ErrRecordNotFound:
			newSub := database.Subscription{
				UserID:    userID,
				PlanType:  plan.Code,
				StartDate: time.Now(),
				EndDate:   time.Now().AddDate(0, 0, plan.DurationDays),
				IsActive:  true,
			}
			tx.Create(&newSub)

			// Выбор оптимального сервера (наименее загруженного)
			var server database.VPNServer
			if err := tx.Where("is_active = ? AND current_users < max_users", true).Order("load_percent ASC").First(&server).Error; err != nil {
				return fmt.Errorf("no available vpn servers: %w", err)
			}

			// Генерация и сохранение конфигурации
			newUUID := uuid.New().String()
			config := database.VPNConfig{
				UserID:         userID,
				SubscriptionID: newSub.ID,
				ServerID:       server.ID,
				UUID:           newUUID,
				Flow:           "xtls-rprx-vision",
			}
			tx.Create(&config)

			// Обновление нагрузки на сервер
			server.CurrentUsers += 1
			server.UpdateLoad()
			tx.Save(&server)

			if m.xrayClient != nil {
				if err := m.xrayClient.AddUser(context.Background(), newUUID, newUUID); err != nil {
					m.log.WithError(err).WithField("uuid", newUUID).Error("Failed to inject user into Xray")
				} else {
					m.log.WithField("uuid", newUUID).Info("Successfully injected user into Xray")
				}
			} else {
				m.log.Warn("XrayClient is nil, user saved in DB but not added to Xray")
			}
			
			m.log.WithFields(logrus.Fields{"user_id": userID, "uuid": newUUID, "server": server.Name}).Info("Provisioned new VPN access")
		default:
			return err
		}
		return nil
	})
}