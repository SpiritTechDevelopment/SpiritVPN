package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/vpn"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testAccessIssuerStub struct {
	identity vpn.Identity
	profile  vpn.ClientProfile
}

func (s *testAccessIssuerStub) IssueTestAccess(_ context.Context, identity vpn.Identity) (vpn.ClientProfile, error) {
	s.identity = identity
	return s.profile, nil
}

func testAccessRouter(issuer testAccessIssuer) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	registerInternalRoutes(router, "test-token", issuer)
	return router
}

func TestIssueTestAccessEndpoint(t *testing.T) {
	expiresAt := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	issuer := &testAccessIssuerStub{profile: vpn.ClientProfile{
		URI: "vless://profile", UUID: "550e8400-e29b-41d4-a716-446655440000", ExpiresAt: expiresAt,
	}}
	request := httptest.NewRequest(http.MethodPost, TestAccessRoute, strings.NewReader(`{"telegram_id":42,"username":"roman"}`))
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	testAccessRouter(issuer).ServeHTTP(response, request)

	assert.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, vpn.Identity{TelegramID: 42, Username: "roman"}, issuer.identity)
	assert.JSONEq(t, `{"uri":"vless://profile","uuid":"550e8400-e29b-41d4-a716-446655440000","expires_at":"2026-07-23T12:00:00Z"}`, response.Body.String())
}

func TestIssueTestAccessEndpointRejectsMissingToken(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, TestAccessRoute, strings.NewReader(`{"telegram_id":42}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	testAccessRouter(&testAccessIssuerStub{}).ServeHTTP(response, request)

	require.Equal(t, http.StatusUnauthorized, response.Code)
}
