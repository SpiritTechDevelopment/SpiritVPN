package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/RomanRyabinkin/SpiritVPN/internal/bot"
	"github.com/RomanRyabinkin/SpiritVPN/internal/database"
	"github.com/RomanRyabinkin/SpiritVPN/pkg/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	db, err := database.Connect(cfg)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer func () { _ = db.Close() } ()

	botAPI, err := tgbotapi.NewBotAPI(cfg.Telegram.BotToken)
	if err != nil {
		log.Fatalf("Failed to create bot: %v", err)
	}

	botAPI.Debug = cfg.Telegram.Debug

	log.Printf("Authorized on account %s", botAPI.Self.UserName)

	botHandler := bot.NewBot(cfg, db, botAPI)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		if err := botHandler.Start(ctx); err != nil {
			log.Fatalf("Failed to start bot: %v", err)
		}
	}()

	log.Println("Telegram Bot started successfully")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	<-sigCh
	log.Println("Shutting down Telegram bot...")

	botHandler.Stop()

	log.Println("Telegram Bot stopped")
}
