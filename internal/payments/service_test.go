package payments_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/database"
	"github.com/RomanRyabinkin/SpiritVPN/internal/payments"
	"github.com/RomanRyabinkin/SpiritVPN/pkg/config"
	"github.com/RomanRyabinkin/SpiritVPN/pkg/logger"
	"github.com/stretchr/testify/suite"
)

// Имитирует платежный шлюз без реальных сетевых запросов
type MockProvider struct {
	MockInvoiceURL string
	MockInvoiceErr error
	MockVerifyErr  error
	MockPayload    *payments.WebhookPayload
}

func (m *MockProvider) CreateInvoice(ctx context.Context, orderID string, amount float64, currency string) (string, error) {
	return m.MockInvoiceURL, m.MockInvoiceErr
}

func (m *MockProvider) VerifyWebhook(rawBody []byte, signature string) (*payments.WebhookPayload, error) {
	if m.MockVerifyErr != nil {
		return nil, m.MockVerifyErr
	}
	return m.MockPayload, nil
}

func (m *MockProvider) Name() string { return "mock_provider" }

type grantCall struct {
	userID   uint
	planCode string
}

type MockAccessGranter struct {
	calls chan grantCall
	err   error
}

func (m *MockAccessGranter) GrantAccess(_ context.Context, userID uint, planCode string) error {
	m.calls <- grantCall{userID: userID, planCode: planCode}
	return m.err
}

type PaymentServiceTestSuite struct {
	suite.Suite
	db      *database.DB
	service *payments.Service
	mock    *MockProvider
	granter *MockAccessGranter
	user    *database.User
}

func (s *PaymentServiceTestSuite) SetupSuite() {
	if testing.Short() {
		s.T().Skip("skipping PostgreSQL integration tests in local mode")
	}

	_ = logger.Setup(&logger.Config{Enabled: false})

	cfg := &config.Config{Database: config.DatabaseConfig{Host: "localhost", Port: 5432, User: "spiritdb", Password: "your_secure_password", Name: "spiritdb"}}
	db, err := database.Connect(cfg)
	s.Require().NoError(err)

	err = database.Migrate(db)
	s.Require().NoError(err)

	s.db = db

	s.mock = &MockProvider{MockInvoiceURL: "https://mock.pay/123"}
	s.granter = &MockAccessGranter{calls: make(chan grantCall, 10)}
	s.service = payments.NewService(db, s.mock, logger.GetLogger("test"), s.granter)
}

func (s *PaymentServiceTestSuite) SetupTest() {
	s.mock.MockInvoiceErr = nil
	s.mock.MockVerifyErr = nil
	s.mock.MockPayload = nil
	for len(s.granter.calls) > 0 {
		<-s.granter.calls
	}
	s.user = &database.User{TelegramID: time.Now().UnixNano(), Username: "payer"}
	s.Require().NoError(s.db.GetDB().Create(s.user).Error)
}

func (s *PaymentServiceTestSuite) TearDownTest() {
	if s.user == nil {
		return
	}
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
	s.Equal("RUB", payment.Currency)
	s.Equal("mock_provider", payment.PaymentMethod)
}

func (s *PaymentServiceTestSuite) TestGeneratePaymentLink_ProviderFailure() {
	s.mock.MockInvoiceErr = fmt.Errorf("provider unavailable")

	url, err := s.service.GeneratePaymentLink(context.Background(), s.user.ID, 500, "RUB")

	s.Empty(url)
	s.ErrorContains(err, "provider error")
	var payment database.Payment
	s.Require().NoError(s.db.GetDB().Where("user_id = ?", s.user.ID).First(&payment).Error)
	s.Equal("failed", payment.Status)
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

	select {
	case call := <-s.granter.calls:
		s.Equal(s.user.ID, call.userID)
		s.Equal("premium", call.planCode)
	case <-time.After(time.Second):
		s.Fail("VPN access was not granted")
	}
}

func (s *PaymentServiceTestSuite) TestProcessWebhook_DuplicateIsIdempotent() {
	payment := database.Payment{
		UserID: s.user.ID, Amount: 100, Status: "succeeded", TransactionID: "tx-existing",
	}
	s.Require().NoError(s.db.GetDB().Create(&payment).Error)
	s.mock.MockPayload = &payments.WebhookPayload{
		OrderID: fmt.Sprintf("%d", payment.ID), TransactionID: "tx-duplicate", Status: "paid",
	}

	s.Require().NoError(s.service.ProcessWebhook(context.Background(), nil, "signature"))

	var stored database.Payment
	s.Require().NoError(s.db.GetDB().First(&stored, payment.ID).Error)
	s.Equal("tx-existing", stored.TransactionID)
	select {
	case <-s.granter.calls:
		s.Fail("duplicate webhook granted access twice")
	case <-time.After(50 * time.Millisecond):
	}
}

func (s *PaymentServiceTestSuite) TestProcessWebhook_Failed() {
	payment := database.Payment{UserID: s.user.ID, Amount: 100, Status: "pending"}
	s.Require().NoError(s.db.GetDB().Create(&payment).Error)
	s.mock.MockPayload = &payments.WebhookPayload{OrderID: fmt.Sprintf("%d", payment.ID), Status: "failed"}

	s.Require().NoError(s.service.ProcessWebhook(context.Background(), nil, "signature"))

	var stored database.Payment
	s.Require().NoError(s.db.GetDB().First(&stored, payment.ID).Error)
	s.Equal("failed", stored.Status)
}

func (s *PaymentServiceTestSuite) TestProcessWebhook_VerificationFailure() {
	s.mock.MockVerifyErr = fmt.Errorf("invalid signature")
	err := s.service.ProcessWebhook(context.Background(), nil, "bad-signature")
	s.ErrorContains(err, "invalid signature")
}

func (s *PaymentServiceTestSuite) TestProcessWebhook_InvalidOrderID() {
	s.mock.MockPayload = &payments.WebhookPayload{OrderID: "not-a-number", Status: "paid"}
	err := s.service.ProcessWebhook(context.Background(), nil, "signature")
	s.ErrorContains(err, "invalid order id")
}

func TestPaymentServiceSuite(t *testing.T) {
	suite.Run(t, new(PaymentServiceTestSuite))
}
