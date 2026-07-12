package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/api"
	"github.com/RomanRyabinkin/SpiritVPN/internal/database"
	"github.com/RomanRyabinkin/SpiritVPN/internal/payments"
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
	}
	if err := logger.Setup(loggerCfg); err != nil {
		panic("Failed to setup logger: " + err.Error())
	}

	log := logger.GetLogger("api-server.main")
	log.Info("Starting API Server...")

	db, err := database.Connect(cfg)
	if err != nil {
		log.WithError(err).Fatal("Failed to connect to database")
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.WithError(err).Error("Error closing database")
		}
	}()

	if err := database.Migrate(db); err != nil {
		log.WithError(err).Fatal("Failed to run migrations")
	}

	xrayClient, err := vpn.NewXrayClient(cfg.VPN.ApiAddress, cfg.VPN.ApiPort, "vless-inbound")
	if err != nil {
		log.WithError(err).Warn("Could not connect to Xray API (is it running?)")
	} else {
		defer func () { _ = xrayClient.Close() } ()
		log.Info("Connected to Xray API successfully")
	}
	
	vpnManager := vpn.NewManager(db, xrayClient)

	// ==========================================
	// Инициализация CryptoPay 
	// ==========================================
	cryptoToken := os.Getenv("CRYPTOPAY_TOKEN")
	if cryptoToken == "" {
		log.Warn("CRYPTOPAY_TOKEN is empty! Payments will fail.")
	}
	cryptoProvider := payments.NewCryptoPayProvider(cryptoToken, false)
	
	paymentLog := logger.GetLogger("payments.service")
	paymentService := payments.NewService(db, cryptoProvider, paymentLog, vpnManager)

	server := api.NewServer(cfg, db, paymentService)

	httpServer := &http.Server{
		Addr:         cfg.API.Address,
		Handler:      server.Router(),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Infof("API Server listening on %s", cfg.API.Address)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.WithError(err).Fatal("Failed to start API server")
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	
	log.Info("Shutting down API server...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		log.WithError(err).Error("Error during shutdown")
	}
	log.Info("API Server stopped")
}