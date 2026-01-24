package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/database"
	"github.com/RomanRyabinkin/SpiritVPN/internal/vpn"
	"github.com/RomanRyabinkin/SpiritVPN/pkg/config"
	"github.com/RomanRyabinkin/SpiritVPN/pkg/logger"
)

func main() {

	cfg, err := config.Load()
	if err != nil {
		panic("Failed to load config: " + err.Error())
	}

	loggerCfg := &logger.Config{
		Level:            cfg.Logger.Level,
		LogDir:           cfg.Logger.LogDir,
		ConsoleOutput:    cfg.Logger.ConsoleOutput,
		FileOutput:       cfg.Logger.FileOutput,
		ColoredOutput:    cfg.Logger.ColoredOutput,
		ErrorLogFile:     cfg.Logger.ErrorLogFile,
		Enabled:          cfg.Logger.Enabled,
		TimestampFormat:  "2006-01-02T15:04:05Z07:00",
		MaxFileSize:      cfg.Logger.MaxFileSize,
		MaxBackups:       cfg.Logger.MaxBackups,
		MaxAge:           cfg.Logger.MaxAge,
		TelegramBotToken: cfg.Logger.TelegramBotToken,
		TelegramChatID:   cfg.Logger.TelegramChatID,
		TelegramThreadID: cfg.Logger.TelegramThreadID,
	}
	if err := logger.Setup(loggerCfg); err != nil {
		panic("Failed to setup logger: " + err.Error())
	}

	log := logger.GetLogger("vpn-server.main")
	log.Info("Starting VPN Server...")

	db, err := database.Connect(cfg)
	if err != nil {
		log.WithError(err).Fatal("Failed to connect to database")
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.WithError(err).Error("Error closing database")
		}
	}()

	server, err := vpn.NewServer(cfg, db)
	if err != nil {
		log.WithError(err).Fatal("Failed to create VPN server")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := server.Start(ctx); err != nil {
		log.WithError(err).Fatal("Failed to start VPN server")
	}

	log.WithField("port", cfg.VPN.Port).Info("VPN Server started successfully")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	<-sigCh
	log.Info("Shutting down VPN server...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := server.Stop(shutdownCtx); err != nil {
		log.WithError(err).Error("Error during shutdown")
	}

	log.Info("VPN Server stopped")
}
