package payments

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/database"
	"github.com/RomanRyabinkin/SpiritVPN/internal/vpn"
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// Инкапсулирует логику обработки платежей.
type Service struct {
	db         *database.DB
	provider   Provider
	log        *logrus.Entry
	vpnManager *vpn.Manager
}

// Создает новый экземпляр платежного сервиса.
func NewService(db *database.DB, provider Provider, log *logrus.Entry, vpnManager *vpn.Manager) *Service {
	return &Service{db: db, provider: provider, log: log, vpnManager: vpnManager}
}

// Создает запись о платеже в статусе 'pending' 
// и запрашивает у провайдера (например, Cryptomus) URL для оплаты.
func (s *Service) GeneratePaymentLink(ctx context.Context, userID uint, amount float64, currency string) (string, error) {
	payment := &database.Payment{
		UserID:        userID,
		Amount:        amount,
		Currency:      currency,
		Status:        "pending",
		PaymentMethod: s.provider.Name(),
		TransactionID: fmt.Sprintf("pending-%d", time.Now().UnixNano()),
	}

	if err := s.db.GetDB().Create(payment).Error; err != nil {
		return "", fmt.Errorf("failed to save payment: %w", err)
	}

	orderID := strconv.FormatUint(uint64(payment.ID), 10)

	url, err := s.provider.CreateInvoice(ctx, orderID, amount, currency)
	if err != nil {
		s.db.GetDB().Model(payment).Update("status", "failed")
		return "", fmt.Errorf("provider error: %w", err)
	}

	return url, nil
}

// Обрабатывает уведомления от платежного шлюза.
// Использует блокировку строк (FOR UPDATE) базы данных для предотвращения 
// двойных начислений при race conditions.
func (s *Service) ProcessWebhook(ctx context.Context, rawBody []byte, signature string) error {
	payload, err := s.provider.VerifyWebhook(rawBody, signature)
	if err != nil {
		s.log.WithError(err).Warn("Webhook verification failed")
		return err
	}

	orderID, _ := strconv.ParseUint(payload.OrderID, 10, 32)

	return s.db.GetDB().Transaction(func(tx *gorm.DB) error {
		var payment database.Payment
		
		// Блокирует строку от изменений другими транзакциями
		if err := tx.Set("gorm:query_option", "FOR UPDATE").First(&payment, orderID).Error; err != nil {
			return err
		}

		if payment.Status == "succeeded" {
			s.log.WithField("order_id", orderID).Info("Payment already processed, ignoring duplicate webhook")
			return nil
		}

		if payload.Status == "paid" {
			payment.Status = "succeeded"
			payment.TransactionID = payload.TransactionID
			
			if err := tx.Save(&payment).Error; err != nil {
				return err
			}

			// Асинхронная выдача VPN доступа
			go func(userID uint) {
				// TODO: В будущем брать код тарифа из payment.Metadata
				err := s.vpnManager.GrantAccess(context.Background(), userID, "premium")
				if err != nil {
					s.log.WithError(err).WithField("user_id", userID).Error("Failed to grant VPN access after payment")
				}
			}(payment.UserID)
			
			s.log.WithField("order_id", orderID).Info("Payment successfully processed!")
			
		} else if payload.Status == "failed" {
			payment.Status = "failed"
			tx.Save(&payment)
		}

		return nil
	})
}