package payments

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func cryptoPaySignature(token string, body []byte) string {
	secret := sha256.Sum256([]byte(token))
	mac := hmac.New(sha256.New, secret[:])
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestCryptoPayProvider_VerifyWebhook(t *testing.T) {
	provider := NewCryptoPayProvider("test-token", true)
	body := []byte(`{"update_type":"invoice_paid","payload":{"invoice_id":42,"status":"paid","payload":"17","amount":"599.00"}}`)

	payload, err := provider.VerifyWebhook(body, cryptoPaySignature("test-token", body))

	require.NoError(t, err)
	assert.Equal(t, "17", payload.OrderID)
	assert.Equal(t, "42", payload.TransactionID)
	assert.Equal(t, "599.00", payload.Amount)
	assert.Equal(t, "paid", payload.Status)
}

func TestCryptoPayProvider_VerifyWebhookRejectsInvalidSignature(t *testing.T) {
	provider := NewCryptoPayProvider("test-token", true)

	payload, err := provider.VerifyWebhook([]byte(`{}`), "invalid")

	assert.Nil(t, payload)
	assert.ErrorContains(t, err, "invalid cryptopay signature")
}

func TestCryptoPayProvider_VerifyWebhookIgnoresOtherUpdates(t *testing.T) {
	provider := NewCryptoPayProvider("test-token", true)
	body := []byte(`{"update_type":"invoice_created","payload":{"payload":"17"}}`)

	payload, err := provider.VerifyWebhook(body, cryptoPaySignature("test-token", body))

	assert.Nil(t, payload)
	assert.ErrorContains(t, err, "ignored update type")
}

func TestCryptoPayProvider_CreateInvoice(t *testing.T) {
	var receivedToken string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		receivedToken = r.Header.Get("Crypto-Pay-API-Token")
		assert.Equal(t, "/api/createInvoice", r.URL.Path)
		return jsonResponse(`{"ok":true,"result":{"pay_url":"https://pay.example/invoice/42"}}`), nil
	})}

	provider := NewCryptoPayProvider("test-token", true)
	provider.client = client

	url, err := provider.CreateInvoice(context.Background(), "17", 599, "RUB")

	require.NoError(t, err)
	assert.Equal(t, "https://pay.example/invoice/42", url)
	assert.Equal(t, "test-token", receivedToken)
}

func TestCryptoPayProvider_CreateInvoiceAPIError(t *testing.T) {
	provider := NewCryptoPayProvider("test-token", true)
	provider.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(`{"ok":false,"error":{"name":"AMOUNT_TOO_SMALL"}}`), nil
	})}

	url, err := provider.CreateInvoice(context.Background(), "17", 1, "RUB")

	assert.Empty(t, url)
	assert.ErrorContains(t, err, "AMOUNT_TOO_SMALL")
}
