package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/vpn"
)

func main() {
	fmt.Println("=== SpiritVPN Xray API Smoke Test ===")
	fmt.Println()

	fmt.Println("1. Connecting to Xray API at 127.0.0.1:10085...")
	client, err := vpn.NewXrayClient("127.0.0.1", 10085, "vless-inbound")
	if err != nil {
		log.Fatalf("[ERROR] Failed to connect to Xray API: %v", err)
	}
	defer func() { _ = client.Close() }()
	fmt.Println("[OK] Connected successfully")

	ctx := context.Background()
	testUUID := "550e8400-e29b-41d4-a716-446655440000"
	testEmail := "test-user@example.com"

	fmt.Printf("\n2. Adding test user (UUID: %s, Email: %s)...\n", testUUID, testEmail)
	err = client.AddUser(ctx, testUUID, testEmail)
	if err != nil {
		log.Fatalf("[ERROR] Failed to add user: %v", err)
	}
	fmt.Println("[OK] User added successfully")

	time.Sleep(1 * time.Second)

	fmt.Printf("\n3. Getting stats for user %s...\n", testEmail)
	received, sent, err := client.GetStats(ctx, testEmail)
	if err != nil {
		log.Printf("[WARNING] Failed to get stats: %v (это нормально, если пользователь еще не передавал трафик)", err)
	} else {
		fmt.Printf("[OK] Stats retrieved: received=%d bytes, sent=%d bytes\n", received, sent)
	}

	fmt.Printf("\n4. Removing test user %s...\n", testEmail)
	err = client.RemoveUser(ctx, testEmail)
	if err != nil {
		log.Fatalf("[ERROR] Failed to remove user: %v", err)
	}
	fmt.Println("[OK] User removed successfully")

	fmt.Println("\n=== All tests passed! ===")
}
