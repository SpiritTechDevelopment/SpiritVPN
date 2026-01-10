//go:build ignore
// +build ignore

package main

import (
	"log"
	"os"

	"github.com/RomanRyabinkin/SpiritVPN/pkg/logger"
)

func main() {
	// Настройка логгера для Telegram Bot'а
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
				"13",           // Errors topic
				"telegram-bot", // Компонент
			)
			logger.Log.AddHook(hook)
		}
	}

	botLog := logger.GetLogger("telegram-bot.main")
	botLog.Info("Telegram Bot starting...")

	// Пример использования с пользовательским контекстом
	userLog := logger.WithUserContext(12345)
	userLog.Info("User started bot")

	// Критическая ошибка отправляется в Telegram с префиксом Bot
	botLog.WithFields(logger.Fields{
		"user_id": 12345,
		"command": "/start",
	}).Fatal("Failed to process command")
}
