package main

import (
	"fmt"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/pkg/config"
	"github.com/RomanRyabinkin/SpiritVPN/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Printf("Failed to load config: %v\n", err)
		return
	}

	loggerConfig := &logger.Config{
		Level:            cfg.Logger.Level,
		LogDir:           cfg.Logger.LogDir,
		ConsoleOutput:    cfg.Logger.ConsoleOutput,
		FileOutput:       cfg.Logger.FileOutput,
		ColoredOutput:    cfg.Logger.ColoredOutput,
		ErrorLogFile:     cfg.Logger.ErrorLogFile,
		Enabled:          cfg.Logger.Enabled,
		TimestampFormat:  time.RFC3339,
		MaxFileSize:      cfg.Logger.MaxFileSize,
		MaxBackups:       cfg.Logger.MaxBackups,
		MaxAge:           cfg.Logger.MaxAge,
		TelegramBotToken: cfg.Logger.TelegramBotToken,
		TelegramChatID:   cfg.Logger.TelegramChatID,
	}

	if err := logger.Setup(loggerConfig); err != nil {
		fmt.Printf("Failed to setup logger: %v\n", err)
		return
	}

	logger.Info("=== SpiritVPN Integration Example ===")

	demonstrateVPNLogging()
	demonstrateDatabaseLogging()
	demonstrateAPILogging(cfg)
	demonstrateBotLogging()

	logger.Info("=== Integration example completed ===")
}

// demonstrateVPNLogging показывает логирование VPN операций
func demonstrateVPNLogging() {
	log := logger.GetLogger("vpn.server")
	log.Info("Initializing VPN server")

	userID := 12345
	email := "user@example.com"

	vpnLog := logger.WithVPNContext(userID, email)
	vpnLog.Info("User connecting to VPN")

	vpnLog.WithFields(logrus.Fields{
		"uuid":   "550e8400-e29b-41d4-a716-446655440000",
		"server": "vpn.example.com",
		"port":   443,
	}).Info("User added to VPN server")

	time.Sleep(100 * time.Millisecond)

	vpnLog.WithFields(logrus.Fields{
		"received_bytes": 1024000,
		"sent_bytes":     512000,
		"duration_sec":   120,
	}).Info("Traffic statistics")

	vpnLog.Info("VPN connection closed")
}

// demonstrateDatabaseLogging показывает логирование операций с БД
func demonstrateDatabaseLogging() {
	log := logger.GetLogger("database")

	log.Info("Connecting to PostgreSQL")

	log.WithFields(logrus.Fields{
		"host":     "localhost",
		"port":     5432,
		"database": "spiritdb",
	}).Info("Database connection established")

	gormLog := logger.NewGormLogger("gorm", 200*time.Millisecond)
	_ = gormLog // В реальном коде используется с gorm.Open()

	log.WithFields(logrus.Fields{
		"query":      "SELECT * FROM users WHERE id = $1",
		"params":     []interface{}{12345},
		"elapsed_ms": 15,
		"rows":       1,
	}).Debug("SQL query executed")

	log.WithFields(logrus.Fields{
		"query":      "SELECT * FROM logs WHERE created_at > $1",
		"elapsed_ms": 350,
		"rows":       10000,
	}).Warn("Slow SQL query detected")
}

// demonstrateAPILogging показывает логирование API запросов
func demonstrateAPILogging(cfg *config.Config) {
	log := logger.GetLogger("api.server")

	log.Infof("Starting API server on %s", cfg.API.Address)

	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(logger.GinMiddleware())

	// Пример обработчика
	router.GET("/api/users/:id", func(c *gin.Context) {
		reqLog := logger.GetLoggerFromGinContext(c)

		userID := c.Param("id")
		reqLog.WithField("user_id", userID).Info("Fetching user data")

		time.Sleep(50 * time.Millisecond)

		c.JSON(200, gin.H{
			"id":     userID,
			"email":  "user@example.com",
			"status": "active",
		})
	})

	log.Info("Simulating HTTP requests...")

	log.Info("API server ready")
}

// demonstrateBotLogging показывает логирование Telegram бота
func demonstrateBotLogging() {
	log := logger.GetLogger("bot.handler")

	log.Info("Starting Telegram bot")

	log.WithFields(logrus.Fields{
		"user_id":   98765,
		"username":  "john_doe",
		"command":   "/start",
		"chat_id":   123456789,
		"chat_type": "private",
	}).Info("Processing bot command")

	time.Sleep(100 * time.Millisecond)

	log.WithFields(logrus.Fields{
		"user_id":  98765,
		"response": "Welcome message sent",
	}).Info("Command processed successfully")

	log.WithFields(logrus.Fields{
		"user_id": 98765,
		"command": "/subscribe",
		"error":   "insufficient balance",
	}).Warn("Command failed: insufficient balance")
}
