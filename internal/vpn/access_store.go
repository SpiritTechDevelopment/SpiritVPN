package vpn

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/database"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// GormAccessStore реализует хранение тестовых VPN-доступов через GORM.
// Для одной пары пользователь–сервер возвращается один доступ.
type GormAccessStore struct {
	db       *database.DB
	endpoint RealityEndpoint
	now      func() time.Time
}

// NewGormAccessStore создаёт GORM-хранилище VPN-доступов для указанной Reality-ноды.
func NewGormAccessStore(db *database.DB, endpoint RealityEndpoint) (*GormAccessStore, error) {
	if db == nil {
		return nil, errors.New("database is required")
	}
	if err := endpoint.Validate(); err != nil {
		return nil, fmt.Errorf("invalid Reality endpoint: %w", err)
	}
	return &GormAccessStore{db: db, endpoint: endpoint, now: time.Now}, nil
}

// GetOrCreate возвращает существующий доступ пользователя к ноде либо атомарно
// создаёт подписку, VPN-конфигурацию и обновляет счётчик занятых мест сервера.
func (s *GormAccessStore) GetOrCreate(
	ctx context.Context,
	identity Identity,
	clientUUID, accountingID string,
	ttl time.Duration,
) (Access, error) {
	var result Access
	err := s.db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		user := database.User{TelegramID: identity.TelegramID}
		if err := tx.Where("telegram_id = ?", identity.TelegramID).FirstOrCreate(&user).Error; err != nil {
			return err
		}
		if identity.Username != "" && user.Username != identity.Username {
			if err := tx.Model(&user).Update("username", identity.Username).Error; err != nil {
				return err
			}
		}

		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, user.ID).Error; err != nil {
			return err
		}

		server := database.VPNServer{Name: s.endpoint.NodeName}
		if err := tx.Where("name = ?", s.endpoint.NodeName).FirstOrCreate(&server, database.VPNServer{
			Name: s.endpoint.NodeName, Host: s.endpoint.Host, Port: s.endpoint.Port,
			PublicKey: s.endpoint.PublicKey, IsActive: true, MaxUsers: 1000,
		}).Error; err != nil {
			return err
		}

		var existing database.VPNConfig
		err := tx.Where("user_id = ? AND server_id = ?", user.ID, server.ID).
			Order("created_at ASC").First(&existing).Error
		if err == nil {
			var subscription database.Subscription
			if err := tx.First(&subscription, existing.SubscriptionID).Error; err != nil {
				return err
			}
			result = Access{
				ConfigID: existing.ID, UUID: existing.UUID, AccountingID: accountingID,
				ExpiresAt: subscription.EndDate, Created: false,
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		if !server.IsActive || !server.HasCapacity() {
			return fmt.Errorf("VPN node %q has no capacity", server.Name)
		}

		now := s.now().UTC()
		subscription := database.Subscription{
			UserID: user.ID, PlanType: "test", StartDate: now, EndDate: now.Add(ttl), IsActive: true,
		}
		if err := tx.Create(&subscription).Error; err != nil {
			return err
		}

		vpnConfig := database.VPNConfig{
			UserID: user.ID, SubscriptionID: subscription.ID, ServerID: server.ID,
			UUID: clientUUID, Flow: defaultVLESSFlow,
		}
		if err := tx.Create(&vpnConfig).Error; err != nil {
			return err
		}

		server.CurrentUsers++
		server.UpdateLoad()
		if err := tx.Save(&server).Error; err != nil {
			return err
		}

		result = Access{
			ConfigID: vpnConfig.ID, UUID: vpnConfig.UUID, AccountingID: accountingID,
			ExpiresAt: subscription.EndDate, Created: true,
		}
		return nil
	})
	if err != nil {
		return Access{}, err
	}
	return result, nil
}

// Delete удаляет VPN-конфигурацию и связанную подписку, после чего освобождает
// занятое место на сервере. Повторное удаление отсутствующей конфигурации безопасно.
func (s *GormAccessStore) Delete(ctx context.Context, configID uint) error {
	return s.db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var vpnConfig database.VPNConfig
		if err := tx.First(&vpnConfig, configID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if err := tx.Delete(&vpnConfig).Error; err != nil {
			return err
		}
		if err := tx.Delete(&database.Subscription{}, vpnConfig.SubscriptionID).Error; err != nil {
			return err
		}
		return tx.Model(&database.VPNServer{}).
			Where("id = ? AND current_users > 0", vpnConfig.ServerID).
			Updates(map[string]any{
				"current_users": gorm.Expr("current_users - 1"),
				"load_percent":  gorm.Expr("(current_users - 1) * 100.0 / NULLIF(max_users, 0)"),
			}).Error
	})
}
