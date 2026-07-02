package payments_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/database"
	"github.com/RomanRyabinkin/SpiritVPN/internal/payments"
	"github.com/RomanRyabinkin/SpiritVPN/internal/vpn"
	"github.com/RomanRyabinkin/SpiritVPN/pkg/config"
	"github.com/RomanRyabinkin/SpiritVPN/pkg/logger"
	"github.com/stretchr/testify/suite"
)

// Имитирует платежный шлюз без реальных сетевых запросов
type MockProvider struct {
	MockInvoiceURL string
	MockVerifyErr  error
	MockPayload    *payments.WebhookPayload
}

func (m *MockProvider) CreateInvoice(ctx context.Context, orderID string, amount float64, currency string) (string, error) {
	return m.MockInvoiceURL, nil
}

func (m *MockProvider) VerifyWebhook(rawBody []byte, signature string) (*payments.WebhookPayload, error) {
	if m.MockVerifyErr != nil {
		return nil, m.MockVerifyErr
	}
	return m.MockPayload, nil
}

func (m *MockProvider) Name() string { return "mock_provider" }


type PaymentServiceTestSuite struct {
	suite.Suite
	db      *database.DB
	service *payments.Service
	mock    *MockProvider
	user    *database.User
}

func (s *PaymentServiceTestSuite) SetupSuite() {
	logger.Setup(&logger.Config{Enabled: false})

	cfg := &config.Config{Database: config.DatabaseConfig{Host: "localhost", Port: 5432, User: "spiritdb", Password: "your_secure_password", Name: "spiritdb"}}
	db, err := database.Connect(cfg)
	s.Require().NoError(err)
	s.db = db

	vpnManager := vpn.NewManager(db, nil) // Без реального Xray
	s.mock = &MockProvider{MockInvoiceURL: "https://mock.pay/123"}
	
	s.service = payments.NewService(db, s.mock, logger.GetLogger("test"), vpnManager)
}

func (s *PaymentServiceTestSuite) SetupTest() {
	s.user = &database.User{TelegramID: time.Now().UnixNano(), Username: "payer"}
	s.db.GetDB().Create(s.user)
}

func (s *PaymentServiceTestSuite) TearDownTest() {
	s.db.GetDB().Exec("DELETE FROM payments WHERE user_id = ?", s.user.ID)
	s.db.GetDB().Exec("DELETE FROM users WHERE id = ?", s.user.ID)
}

// TestGeneratePaymentLink проверяет сохранение платежа в БД и получение ссылки
func (s *PaymentServiceTestSuite) TestGeneratePaymentLink() {
	ctx := context.Background()

	url, err := s.service.GeneratePaymentLink(ctx, s.user.ID, 500, "RUB")
	s.Require().NoError(err)
	s.Equal(s.mock.MockInvoiceURL, url)

	var payment database.Payment
	s.db.GetDB().Where("user_id = ?", s.user.ID).First(&payment)
	
	s.Equal(float64(500), payment.Amount)
	s.Equal("pending", payment.Status)
}

// Проверяет транзакционную обработку вебхука
func (s *PaymentServiceTestSuite) TestProcessWebhook_Success() {
	ctx := context.Background()

	payment := database.Payment{UserID: s.user.ID, Amount: 100, Status: "pending"}
	s.db.GetDB().Create(&payment)
	orderIDStr := fmt.Sprintf("%d", payment.ID)

	s.mock.MockPayload = &payments.WebhookPayload{
		OrderID:       orderIDStr,
		TransactionID: "tx-success-001",
		Status:        "paid",
	}

	err := s.service.ProcessWebhook(ctx, []byte("fake"), "fake_sig")
	s.Require().NoError(err)

	var updatedPayment database.Payment
	s.db.GetDB().First(&updatedPayment, payment.ID)
	
	s.Equal("succeeded", updatedPayment.Status, "Статус должен измениться на успешный")
	s.Equal("tx-success-001", updatedPayment.TransactionID)
}

func TestPaymentServiceSuite(t *testing.T) {
	suite.Run(t, new(PaymentServiceTestSuite))
}