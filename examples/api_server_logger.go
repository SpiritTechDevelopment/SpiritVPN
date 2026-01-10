//go:build ignore
// +build ignore

package main

import (
	"log"
	"os"

	"github.com/RomanRyabinkin/SpiritVPN/pkg/logger"
)

func main() {
	// Настройка логгера для API Server'а
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
	if botToken := os.Getenv("TELEGRAM_BOT_TOKEN"); botToken != "" {
		chatID := os.Getenv("TELEGRAM_CHAT_ID")
		if chatID != "" {
			hook := logger.NewTelegramHook(
				botToken,
				chatID,
				"13",         // Errors topic
				"api-server", // Компонент
			)
			logger.Log.AddHook(hook)
		}
	}

	apiLog := logger.GetLogger("api-server.main")
	apiLog.Info("API Server starting...")

	requestLog := apiLog.WithFields(logger.Fields{
		"request_id": "req-123",
		"method":     "POST",
		"path":       "/api/users",
	})

	requestLog.Info("Processing request")

	apiLog.Fatal("Database connection failed")
}
