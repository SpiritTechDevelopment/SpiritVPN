//go:build ignore
// +build ignore

package main

import (
	"log"
	"os"

	"github.com/RomanRyabinkin/SpiritVPN/pkg/logger"
)

func main() {
	logConfig := logger.Config{
		Level:         "info",
		ConsoleOutput: true,
		FileOutput:    true,
		LogDir:        "./logs",
		ColoredOutput: true,
		ErrorLogFile:  true,
		MaxFileSize:   10,
		MaxBackups:    5,
		MaxAge:        30,
	}

	if err := logger.Setup(&logConfig); err != nil {
		log.Fatalf("Failed to setup logger: %v", err)
	}

	// Добавление Telegram hook'а для критических ошибок → Errors topic (Thread ID: 13)
	if botToken := os.Getenv("LOG_TELEGRAM_BOT_TOKEN"); botToken != "" {
		chatID := os.Getenv("LOG_TELEGRAM_CHAT_ID")
		if chatID != "" {
			hook := logger.NewTelegramHook(
				botToken,
				chatID,
				"13",         // Errors topic
				"vpn-server", // Компонент
			)
			logger.Log.AddHook(hook)
		}
	}

	vpnLog := logger.GetLogger("vpn-server.main")
	vpnLog.Info("VPN Server starting...")

	// Пример использования с VPN контекстом
	clientLog := logger.WithVPNContext(12345, "10.8.0.100")
	clientLog.Info("Client connected")

	// Критическая ошибка отправляется в Telegram с префиксом VPN
	vpnLog.WithFields(logger.Fields{
		"user_id":   12345,
		"client_ip": "10.8.0.100",
		"error":     "xray configuration invalid",
	}).Fatal("Failed to start Xray server")
}
