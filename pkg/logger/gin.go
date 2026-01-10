package logger

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
)

// GinMiddleware создает middleware для логирования HTTP запросов в Gin.
//
// Пример использования:
//
//	router := gin.New()
//	router.Use(logger.GinMiddleware())
func GinMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := uuid.New().String()
		c.Set("request_id", requestID)

		reqLog := GetLogger("api.http", logrus.Fields{
			"request_id": requestID,
			"method":     c.Request.Method,
			"path":       c.Request.URL.Path,
			"ip":         c.ClientIP(),
			"user_agent": c.Request.UserAgent(),
		})

		c.Set("logger", reqLog)

		start := time.Now()
		reqLog.Info("Request started")

		c.Next()

		latency := time.Since(start)
		statusCode := c.Writer.Status()

		logEntry := reqLog.WithFields(logrus.Fields{
			"status":        statusCode,
			"latency_ms":    latency.Milliseconds(),
			"response_size": c.Writer.Size(),
		})

		switch {
		case statusCode >= 500:
			logEntry.Error("Request failed with server error")
		case statusCode >= 400:
			logEntry.Warn("Request completed with client error")
		case statusCode >= 300:
			logEntry.Info("Request redirected")
		default:
			logEntry.Info("Request completed successfully")
		}
	}
}

// GetLoggerFromGinContext извлекает логгер из контекста Gin.
// Если логгер не найден, создает новый.
//
// Пример:
//
//	func MyHandler(c *gin.Context) {
//	    log := logger.GetLoggerFromGinContext(c)
//	    log.Info("Processing request")
//	}
func GetLoggerFromGinContext(c *gin.Context) *logrus.Entry {
	if val, exists := c.Get("logger"); exists {
		if log, ok := val.(*logrus.Entry); ok {
			return log
		}
	}

	return GetLogger("api.http", logrus.Fields{
		"path":   c.Request.URL.Path,
		"method": c.Request.Method,
	})
}

func GetRequestID(c *gin.Context) string {
	if val, exists := c.Get("request_id"); exists {
		if id, ok := val.(string); ok {
			return id
		}
	}
	return ""
}
