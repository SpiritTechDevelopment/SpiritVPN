package api

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/RomanRyabinkin/SpiritVPN/internal/vpn"
	"github.com/gin-gonic/gin"
)

// testAccessIssuer описывает прикладной сервис выдачи тестового VPN-доступа.
type testAccessIssuer interface {
	IssueTestAccess(ctx context.Context, identity vpn.Identity) (vpn.ClientProfile, error)
}

// testAccessRequest описывает входные данные внутреннего API выдачи доступа.
type testAccessRequest struct {
	TelegramID int64  `json:"telegram_id" binding:"required,gt=0"`
	Username   string `json:"username" binding:"omitempty,max=255"`
}

// internalTokenAuth проверяет Bearer token для запросов сервер -> сервер.
func internalTokenAuth(expectedToken string) gin.HandlerFunc {
	return func(c *gin.Context) {
		provided := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		valid := expectedToken != "" && len(provided) == len(expectedToken) &&
			subtle.ConstantTimeCompare([]byte(provided), []byte(expectedToken)) == 1
		if !valid {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		c.Next()
	}
}

// issueTestAccessHandler создаёт обработчик выдачи VPN доступа.
func issueTestAccessHandler(issuer testAccessIssuer) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request testAccessRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
			return
		}

		profile, err := issuer.IssueTestAccess(c.Request.Context(), vpn.Identity{
			TelegramID: request.TelegramID,
			Username:   request.Username,
		})
		if err != nil {
			c.Error(err) //nolint:errcheck // Ошибка сохраняется в контексте для middleware Gin.
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "VPN provisioning unavailable"})
			return
		}

		c.JSON(http.StatusOK, profile)
	}
}
