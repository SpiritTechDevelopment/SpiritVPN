package payments

import (
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Проверка правильности алгоритма подписи.
// Алгоритм Cryptomus: MD5(Base64(Payload) + API_KEY)
func TestCryptomusProvider_GenerateSignature(t *testing.T) {
	// Подготовка тестовых данных
	apiKey := "secret_api_key_123"
	merchantID := "merchant_456"
	payload := `{"order_id":"1","amount":"599.00"}`
	
	provider := NewCryptomusProvider(merchantID, apiKey)

	base64Payload := base64.StdEncoding.EncodeToString([]byte(payload))
	hash := md5.Sum([]byte(base64Payload + apiKey))
	expectedSignature := fmt.Sprintf("%x", hash)

	actualSignature := provider.generateSignature(payload)

	assert.Equal(t, expectedSignature, actualSignature, "Generated signature should match expected MD5 hash")
}

// Проверка успешной валидации реального вебхука.
func TestCryptomusProvider_VerifyWebhook_Success(t *testing.T) {
	apiKey := "test_key"
	provider := NewCryptomusProvider("test_merchant", apiKey)

	rawBody := []byte(`{"order_id":"99","uuid":"tx-777","amount":"599.00","status":"paid"}`)
	
	base64Payload := base64.StdEncoding.EncodeToString(rawBody)
	hash := md5.Sum([]byte(base64Payload + apiKey))
	validSignature := fmt.Sprintf("%x", hash)

	result, err := provider.VerifyWebhook(rawBody, validSignature)

	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, "99", result.OrderID)
	assert.Equal(t, "tx-777", result.TransactionID)
	assert.Equal(t, "paid", result.Status)
}

// Тест на дурака: отправка вебхука с недействительной подписью.
func TestCryptomusProvider_VerifyWebhook_InvalidSignature(t *testing.T) {
	provider := NewCryptomusProvider("test_merchant", "real_secret_key")

	rawBody := []byte(`{"order_id":"1","status":"paid"}`) // Фейковый вебхук
	fakeSignature := "e10adc3949ba59abbe56e057f20f883e"   // Придуманная подпись, не соответствующая реальному алгоритму

	result, err := provider.VerifyWebhook(rawBody, fakeSignature)

	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Equal(t, "invalid signature", err.Error(), "Should reject invalid signature")
}

// Проверяет приведение статусов к единому формату.
func TestCryptomusProvider_VerifyWebhook_StatusNormalization(t *testing.T) {
	provider := NewCryptomusProvider("test", "key")
	
	tests := []struct {
		incomingStatus string
		expectedStatus string
	}{
		{"paid", "paid"},
		{"paid_over", "paid"},
		{"cancel", "failed"},
		{"fail", "failed"},
		{"wrong_amount", "pending"},
	}

	for _, tc := range tests {
		t.Run(tc.incomingStatus, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"order_id":"1","status":"%s"}`, tc.incomingStatus))
			sig := provider.generateSignature(string(body))
			
			res, _ := provider.VerifyWebhook(body, sig)
			assert.Equal(t, tc.expectedStatus, res.Status)
		})
	}
}