package payments

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Представляет реализацию интерфейса Provider для интеграции 
// с криптовалютным платежным шлюзом Cryptomus. Обеспечивает взаимодействие с REST API 
// шлюза и криптографическую валидацию входящих уведомлений.
type CryptomusProvider struct {
	merchantID string
	apiKey     string
	client     *http.Client
}

// Инициализирует и возвращает новый экземпляр CryptomusProvider.
// 
// Параметры:
//   - merchantID: уникальный идентификатор мерчанта в системе Cryptomus.
//   - apiKey: секретный ключ для генерации и проверки криптографических подписей.
func NewCryptomusProvider(merchantID, apiKey string) *CryptomusProvider {
	return &CryptomusProvider{
		merchantID: merchantID,
		apiKey:     apiKey,
		client:     &http.Client{Timeout: 10 * time.Second},
	}
}

// Возвращает строковый ID данного платежного провайдера.
// Используется для сохранения информации о методе оплаты в базе данных.
func (c *CryptomusProvider) Name() string {
	return "cryptomus"
}

// Вычисляет криптографическую подпись для тела запроса 
// в соответствии с технической спецификацией API Cryptomus.
// Алгоритм вычисления: MD5(Base64(Payload) + API_KEY).
func (c *CryptomusProvider) generateSignature(payloadStr string) string {
	base64Payload := base64.StdEncoding.EncodeToString([]byte(payloadStr))
	hash := md5.Sum([]byte(base64Payload + c.apiKey))
	return fmt.Sprintf("%x", hash)
}

// Фрмирует запрос к API Cryptomus для создания нового платежного инвойса.
// 
// Параметры:
//   - ctx: контекст запроса для управления таймаутами.
//   - orderID: внутренний идентификатор заказа в системе.
//   - amount: сумма платежа.
//   - currency: фиатная или криптовалютная валюта платежа (например, "RUB", "USD").
// 
// Возвращает:
//   - string: URL-адрес платежной страницы для перенаправления клиента.
//   - error: объект ошибки в случае неудачного сетевого запроса или ответа API.
func (c *CryptomusProvider) CreateInvoice(ctx context.Context, orderID string, amount float64, currency string) (string, error) {
	url := "https://api.cryptomus.com/v1/payment"

	reqBody := map[string]interface{}{
		"amount":   fmt.Sprintf("%.2f", amount),
		"currency": currency,
		"order_id": orderID,
	}

	jsonPayload, _ := json.Marshal(reqBody)
	signature := c.generateSignature(string(jsonPayload))

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("merchant", c.merchantID)
	req.Header.Set("sign", signature)

	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		State  int `json:"state"`
		Result struct {
			URL string `json:"url"`
		} `json:"result"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if result.State != 0 {
		return "", fmt.Errorf("cryptomus API error, state: %d", result.State)
	}

	return result.Result.URL, nil
}

// Осуществляет валидацию входящего HTTP-запроса (вебхука) от Cryptomus.
// Проверяет корректность криптографической подписи для защиты от подделки запросов 
// и выполняет нормализацию специфичных статусов Cryptomus во внутреннюю структуру WebhookPayload.
// 
// Параметры:
//   - rawBody: тело входящего HTTP-запроса в виде массива байт.
//   - signature: значение заголовка "sign", переданного платежным шлюзом.
// 
// Возвращает:
//   - *WebhookPayload: нормализованные данные о платеже.
//   - error: ошибку, если подпись недействительна или структура JSON некорректна.
func (c *CryptomusProvider) VerifyWebhook(rawBody []byte, signature string) (*WebhookPayload, error) {
	expectedSignature := c.generateSignature(string(rawBody))
	if signature != expectedSignature {
		return nil, fmt.Errorf("invalid signature")
	}

	var payload struct {
		OrderID string `json:"order_id"`
		UUID    string `json:"uuid"`
		Amount  string `json:"amount"`
		Status  string `json:"status"`
	}

	if err := json.Unmarshal(rawBody, &payload); err != nil {
		return nil, err
	}

	status := "pending"
	if payload.Status == "paid" || payload.Status == "paid_over" {
		status = "paid"
	} else if payload.Status == "cancel" || payload.Status == "fail" {
		status = "failed"
	}

	return &WebhookPayload{
		OrderID:       payload.OrderID,
		TransactionID: payload.UUID,
		Amount:        payload.Amount,
		Status:        status,
	}, nil
}