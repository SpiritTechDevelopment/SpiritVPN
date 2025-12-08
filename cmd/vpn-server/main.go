package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/vpn"
	"github.com/RomanRyabinkin/SpiritVPN/pkg/config"
)

func main() {

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	server, err := vpn.NewServer(cfg)
	if err != nil {
		log.Fatalf("Failed to create VPN server: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := server.Start(ctx); err != nil {
		log.Fatalf("Failed to start VPN server: %v", err)
	}

	log.Println("VPN Server started successfully")
	log.Printf("Listening on port %d", cfg.VPN.Port)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	<-sigCh
	log.Println("Shutting down VPN server...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := server.Stop(shutdownCtx); err != nil {
		log.Printf("Error during shutdown: %v", err)
	}

	log.Println("VPN Server stopped")
}
