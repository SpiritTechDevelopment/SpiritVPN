package vpn

import (
	"context"
	"fmt"
	"log"

	"github.com/RomanRyabinkin/SpiritVPN/pkg/config"
)

// Server представляет VPN сервер
type Server struct {
	config *config.Config
	// TODO: добавить WireGuard интерфейс
	// wg *wireguard.Device
}

// NewServer создает новый VPN сервер
func NewServer(cfg *config.Config) (*Server, error) {
	server := &Server{
		config: cfg,
	}

	// TODO: Инициализация WireGuard

	return server, nil
}

// Start запускает VPN сервер
func (s *Server) Start(ctx context.Context) error {
	log.Printf("Starting VPN server on port %d", s.config.VPN.Port)

	// TODO: Настройка WireGuard интерфейса
	// TODO: Настройка маршрутизации (iptables)
	// TODO: Запуск мониторинга

	go s.monitorConnections(ctx)

	return nil
}

// Stop останавливает VPN сервер
func (s *Server) Stop(ctx context.Context) error {
	log.Println("Stopping VPN server...")

	// TODO: Остановка WireGuard
	// TODO: Очистка iptables правил
	// TODO: Отключение всех клиентов

	return nil
}

// AddPeer добавляет нового пира (клиента)
func (s *Server) AddPeer(publicKey, allowedIPs string) error {
	// TODO: Добавление пира в WireGuard
	log.Printf("Adding peer: %s with IPs: %s", publicKey, allowedIPs)
	return nil
}

// RemovePeer удаляет пира
func (s *Server) RemovePeer(publicKey string) error {
	// TODO: Удаление пира из WireGuard
	log.Printf("Removing peer: %s", publicKey)
	return nil
}

// GetPeerStats возвращает статистику пира
func (s *Server) GetPeerStats(publicKey string) (*PeerStats, error) {
	// TODO: Получение статистики из WireGuard
	return &PeerStats{
		PublicKey:     publicKey,
		BytesReceived: 0,
		BytesSent:     0,
		LastHandshake: nil,
	}, nil
}

// monitorConnections мониторит активные соединения
func (s *Server) monitorConnections(ctx context.Context) {
	// TODO: Периодическая проверка статуса соединений
	// TODO: Сбор метрик
	// TODO: Логирование событий

	<-ctx.Done()
	log.Println("Connection monitoring stopped")
}

// PeerStats статистика пира
type PeerStats struct {
	PublicKey     string
	BytesReceived uint64
	BytesSent     uint64
	LastHandshake *int64
}

// GenerateKeys генерирует приватный и публичный ключи
func GenerateKeys() (privateKey, publicKey string, err error) {
	// TODO: Генерация WireGuard ключей
	return "", "", fmt.Errorf("not implemented")
}
