package logger_test

import (
	"testing"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/pkg/logger"
	"github.com/sirupsen/logrus"
)

func TestLoggerSetup(t *testing.T) {
	config := &logger.Config{
		Level:           "debug",
		LogDir:          "./test_logs",
		ConsoleOutput:   true,
		FileOutput:      false, // Отключен для теста
		ColoredOutput:   true,
		ErrorLogFile:    false,
		Enabled:         true,
		TimestampFormat: time.RFC3339,
		MaxFileSize:     10,
		MaxBackups:      5,
		MaxAge:          30,
	}

	err := logger.Setup(config)
	if err != nil {
		t.Fatalf("Failed to setup logger: %v", err)
	}

	logger.Debug("Debug message")
	logger.Info("Info message")
	logger.Warn("Warning message")
	logger.Error("Error message")
}

func TestGetLogger(t *testing.T) {
	// Инициализируем логгер
	logger.Setup(logger.DefaultConfig())

	log := logger.GetLogger("test.module", logrus.Fields{
		"test_id": 123,
		"action":  "test_run",
	})

	log.Info("Testing logger with context")
	log.WithField("extra", "value").Warn("Warning with extra field")
}

func TestLoggerUtils(t *testing.T) {
	logger.Setup(logger.DefaultConfig())
	log := logger.GetLogger("test.utils")

	logger.LogTestStart(log, "TestExample", map[string]interface{}{
		"param1": "value1",
		"param2": 42,
	})

	time.Sleep(100 * time.Millisecond)

	logger.LogTestEnd(log, "TestExample", "PASS", 100*time.Millisecond)
}

func TestLoggerWithContext(t *testing.T) {
	logger.Setup(logger.DefaultConfig())

	// User context
	userLog := logger.WithUserContext(12345)
	userLog.Info("User logged in")

	// VPN context
	vpnLog := logger.WithVPNContext(12345, "user@example.com")
	vpnLog.Info("VPN connection established")

	// Request context
	reqLog := logger.WithRequestContext("GET", "/api/users", "req-123")
	reqLog.Info("Handling HTTP request")
}

func TestColorFormatter(t *testing.T) {
	config := &logger.Config{
		Level:           "debug",
		ConsoleOutput:   true,
		FileOutput:      false,
		ColoredOutput:   true,
		Enabled:         true,
		TimestampFormat: "2006-01-02 15:04:05",
	}

	err := logger.Setup(config)
	if err != nil {
		t.Fatalf("Failed to setup logger: %v", err)
	}

	logger.Debug("Debug message in cyan")
	logger.Info("Info message in green")
	logger.Warn("Warning message in yellow")
	logger.Error("Error message in red")
}

func TestLoggerDisabled(t *testing.T) {
	config := &logger.Config{
		Enabled: false,
	}

	err := logger.Setup(config)
	if err != nil {
		t.Fatalf("Failed to setup logger: %v", err)
	}

	// Эти логи не должны выводиться
	logger.Info("This should not appear")
	logger.Error("This should not appear either")
}
