package logger

import (
	"time"

	"github.com/sirupsen/logrus"
)

// Утилитные функции для удобного логирования.

// LogTestStart логирует начало теста с параметрами.
func LogTestStart(log *logrus.Entry, testName string, params map[string]interface{}) {
	log.Info("============================================================")
	log.Infof("Начало теста: %s", testName)
	for key, value := range params {
		log.Infof("  %s: %v", key, value)
	}
	log.Info("============================================================")
}

// LogTestEnd логирует окончание теста с результатом.
func LogTestEnd(log *logrus.Entry, testName string, status string, duration time.Duration) {
	log.Info("============================================================")
	log.Infof("Конец теста: %s - %s", testName, status)
	if duration > 0 {
		log.Infof("  Длительность: %.2fs", duration.Seconds())
	}
	log.Info("============================================================\n")
}

// LogCommand логирует выполняемую команду.
func LogCommand(log *logrus.Entry, command string) {
	log.Debugf("Выполнение команды: %s", command)
}

// LogResponse логирует HTTP ответ.
func LogResponse(log *logrus.Entry, statusCode int, body string, maxLength int) {
	if len(body) > maxLength {
		body = body[:maxLength] + "..."
	}
	log.Debugf("HTTP %d: %s", statusCode, body)
}

// WithUserContext создает логгер с контекстом пользователя
func WithUserContext(userID interface{}) *logrus.Entry {
	return WithContext(logrus.Fields{
		"user_id": userID,
	})
}

// WithRequestContext создает логгер с контекстом HTTP запроса
func WithRequestContext(method, path string, requestID string) *logrus.Entry {
	return WithContext(logrus.Fields{
		"method":     method,
		"path":       path,
		"request_id": requestID,
	})
}

// WithVPNContext создает логгер с контекстом VPN операций
func WithVPNContext(userID interface{}, email string) *logrus.Entry {
	return WithContext(logrus.Fields{
		"user_id": userID,
		"email":   email,
		"service": "vpn",
	})
}
