package vpn

import (
	"context"
	"fmt"
	"log"

	"github.com/RomanRyabinkin/SpiritVPN/pkg/config"
	"github.com/google/uuid"
)

// Server представляет VPN сервер на базе Xray (VLESS).
// Управляет пользователями и статистикой через Xray API.
type Server struct {
	config     *config.Config
	xrayClient *XrayClient
}

// GenerateUUID генерирует новый UUID для VLESS клиента.
func GenerateUUID() (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// NewServer создает и инициализирует новый VPN сервер.
//
// Параметры:
//   - cfg: конфигурация VPN сервера
//
// Возвращает:
//   - *Server: инициализированный VPN сервер
//   - error: ошибка инициализации или nil при успехе
func NewServer(cfg *config.Config) (*Server, error) {
	server := &Server{
		config: cfg,
	}
	return server, nil
}

// Start запускает VPN сервер (или подключается к нему).
//
// Параметры:
//   - ctx: контекст для graceful shutdown
//
// Возвращает:
//   - error: ошибка запуска или nil при нормальной остановке
func (s *Server) Start(ctx context.Context) error {
	log.Printf("Starting VPN server integration on port %d", s.config.VPN.Port)

	// Инициализация подключения к Xray API
	client, err := NewXrayClient(s.config.VPN.ApiAddress, s.config.VPN.ApiPort, "vless-inbound")
	if err != nil {
		return fmt.Errorf("failed to connect to xray api: %w", err)
	}
	s.xrayClient = client
	log.Println("Connected to Xray API")

	go s.monitorConnections(ctx)

	return nil
}

// Stop корректно останавливает работу с сервером.
//
// Параметры:
//   - ctx: контекст с таймаутом для graceful shutdown
//
// Возвращает:
//   - error: ошибка остановки или nil при успехе
func (s *Server) Stop(ctx context.Context) error {
	log.Println("Stopping VPN server integration...")
	if s.xrayClient != nil {
		return s.xrayClient.Close()
	}
	return nil
}

// AddUser добавляет пользователя в конфигурацию Xray/VLESS.
//
// Параметры:
//   - uuid: UUID пользователя
//
// Возвращает:
//   - error: ошибка добавления или nil при успехе
func (s *Server) AddUser(uuid string) error {
	if s.xrayClient == nil {
		return fmt.Errorf("xray client is not initialized")
	}
	// Используем UUID как email для идентификации в Xray
	return s.xrayClient.AddUser(context.Background(), uuid, uuid)
}

// RemoveUser удаляет пользователя из Xray/VLESS.
//
// Параметры:
//   - uuid: UUID пользователя для удаления
//
// Возвращает:
//   - error: ошибка удаления или nil при успехе
func (s *Server) RemoveUser(uuid string) error {
	if s.xrayClient == nil {
		return fmt.Errorf("xray client is not initialized")
	}
	return s.xrayClient.RemoveUser(context.Background(), uuid)
}

// GetUserStats возвращает статистику использования для конкретного пользователя.
//
// Параметры:
//   - uuid: UUID пользователя
//
// Возвращает:
//   - *UserStats: статистика пользователя
//   - error: ошибка получения или nil при успехе
func (s *Server) GetUserStats(uuid string) (*UserStats, error) {
	// TODO: Получение статистики из Xray API (StatsService)
	return &UserStats{
		UUID:          uuid,
		BytesReceived: 0,
		BytesSent:     0,
	}, nil
}

// monitorConnections периодически проверяет статус и собирает метрики.
func (s *Server) monitorConnections(ctx context.Context) {
	// TODO: Периодический сбор статистики через Xray API
	<-ctx.Done()
	log.Println("Connection monitoring stopped")
}

// UserStats содержит статистику использования VPN для конкретного пользователя.
type UserStats struct {
	UUID          string // UUID пользователя
	BytesReceived uint64 // Байт получено (downlink)
	BytesSent     uint64 // Байт отправлено (uplink)
}
