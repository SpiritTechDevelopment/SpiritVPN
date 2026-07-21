package payments

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// CryptoPayProvider реализует интеграцию с платежным шлюзом @CryptoBot (Telegram).
// Поддерживает как основную сеть, так и Testnet для безопасного тестирования.
type CryptoPayProvider struct {
	token  string
	apiURL string
	client *http.Client
}

// NewCryptoPayProvider инициализирует провайдер.
// isTestnet = true направляет запросы на тестовый сервер (бесплатные тестовые монеты).
func NewCryptoPayProvider(token string, isTestnet bool) *CryptoPayProvider {
	apiURL := "https://pay.crypt.bot/api"
	if isTestnet {
		apiURL = "https://testnet-pay.crypt.bot/api"
	}
	return &CryptoPayProvider{
		token:  token,
		apiURL: apiURL,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Name возвращает системный идентификатор провайдера.
func (c *CryptoPayProvider) Name() string {
	return "cryptopay"
}

// CreateInvoice запрашивает у CryptoPay ссылку на оплату.
func (c *CryptoPayProvider) CreateInvoice(ctx context.Context, orderID string, amount float64, currency string) (string, error) {
	// CryptoPay поддерживает фиат. Мы просим выставить счет в рублях (RUB), 
	// а юзер оплатит его криптой по текущему курсу бота.
	reqBody := map[string]interface{}{
		"currency_type": "fiat",
		"fiat":          currency,
		"amount":        fmt.Sprintf("%.2f", amount),
		"description":   "Оплата VPN подписки SpiritVPN",
		"payload":       orderID, // Прячем сюда наш ID платежа из БД, CryptoPay вернет его в вебхуке
	}

	jsonPayload, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, "POST", c.apiURL+"/createInvoice", bytes.NewBuffer(jsonPayload))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Crypto-Pay-API-Token", c.token)

	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() } ()

	var result struct {
		Ok     bool `json:"ok"`
		Result struct {
			PayURL string `json:"pay_url"`
		} `json:"result"`
		Error struct {
			Name string `json:"name"`
		} `json:"error"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if !result.Ok {
		return "", fmt.Errorf("cryptopay API error: %s", result.Error.Name)
	}

	return result.Result.PayURL, nil
}

// VerifyWebhook проверяет криптографическую подпись и парсит уведомление об оплате от CryptoPay.
func (c *CryptoPayProvider) VerifyWebhook(rawBody []byte, signature string) (*WebhookPayload, error) {
	// Алгоритм подписи CryptoPay: HMAC-SHA256(secret, body), где secret = SHA256(token)
	tokenHash := sha256.Sum256([]byte(c.token))
	
	mac := hmac.New(sha256.New, tokenHash[:])
	mac.Write(rawBody)
	expectedSignature := hex.EncodeToString(mac.Sum(nil))

	if signature != expectedSignature {
		return nil, fmt.Errorf("invalid cryptopay signature")
	}

	var webhookData struct {
		UpdateType string `json:"update_type"`
		Payload    struct {
			InvoiceID int64  `json:"invoice_id"`
			Status    string `json:"status"`
			Payload   string `json:"payload"` 
			Amount    string `json:"amount"`
		} `json:"payload"`
	}

	if err := json.Unmarshal(rawBody, &webhookData); err != nil {
		return nil, err
	}

	// Нас интересует только успешная оплата
	if webhookData.UpdateType != "invoice_paid" {
		return nil, fmt.Errorf("ignored update type: %s", webhookData.UpdateType)
	}

	return &WebhookPayload{
		OrderID:       webhookData.Payload.Payload, // Наш внутренний ID
		TransactionID: fmt.Sprintf("%d", webhookData.Payload.InvoiceID),
		Amount:        webhookData.Payload.Amount,
		Status:        "paid", // invoice_paid гарантирует, что оплата прошла
	}, nil
}