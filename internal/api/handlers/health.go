package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/database"
	"github.com/gin-gonic/gin"
)

// HealthCheckResponse представляет ответ health check эндпоинта
type HealthCheckResponse struct {
	Status  string            `json:"status"`
	Service string            `json:"service"`
	Checks  map[string]string `json:"checks,omitempty"`
}

// HealthCheck обрабатывает GET /health эндпоинт.
// Проверка без зависимостей - возвращает 200 OK.
// Используется для Docker health checks и мониторинга.
//
// Параметры:
//   - c: Gin контекст запроса
//
// Возвращает:
//   - 200 OK: сервис работает нормально
func HealthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, HealthCheckResponse{
		Status:  "ok",
		Service: "spiritvpn-api",
	})
}

// HealthCheckAdvanced обрабатывает GET /health/advanced эндпоинт.
// Расширенная проверка с проверкой БД и других зависимостей.
//
// Параметры:
//   - db: подключение к базе данных для проверки
//
// Возвращает:
//   - gin.HandlerFunc: handler функция для регистрации в router
//
// Response:
//   - 200 OK: все проверки пройдены успешно
//   - 503 Service Unavailable: одна или несколько проверок не прошли
func HealthCheckAdvanced(db *database.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		checks := make(map[string]string)
		status := http.StatusOK

		// Проверка подключения к БД
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()

		if err := db.GetDB().WithContext(ctx).Exec("SELECT 1").Error; err != nil {
			checks["database"] = "error: " + err.Error()
			status = http.StatusServiceUnavailable
		} else {
			checks["database"] = "ok"
		}

		// Здесь можно добавить проверки других сервисов, например:
		// - Redis подключение
		// - VPN сервер доступность
		// - Внешние API

		response := HealthCheckResponse{
			Service: "spiritvpn-api",
			Checks:  checks,
		}

		if status == http.StatusOK {
			response.Status = "ok"
		} else {
			response.Status = "degraded"
		}

		c.JSON(status, response)
	}
}
