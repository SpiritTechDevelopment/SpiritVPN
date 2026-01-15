package vpn

import (
	"context"
	"fmt"

	"github.com/RomanRyabinkin/SpiritVPN/internal/database"
	"github.com/RomanRyabinkin/SpiritVPN/internal/workers"
	"github.com/RomanRyabinkin/SpiritVPN/pkg/config"
	"github.com/RomanRyabinkin/SpiritVPN/pkg/logger"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
)

// Server представляет VPN сервер на базе Xray (VLESS).
// Управляет пользователями и статистикой через Xray API.
type Server struct {
	config      *config.Config
	xrayClient  *XrayClient
	db          *database.DB
	statsWorker *workers.StatsWorker
	log         *logrus.Entry
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
// Args:
//   - cfg: конфигурация VPN сервера
//   - db: подключение к базе данных для сохранения статистики
//
// Returns:
//   - *Server: инициализированный VPN сервер
//   - error: ошибка инициализации или nil при успехе
func NewServer(cfg *config.Config, db *database.DB) (*Server, error) {
	server := &Server{
		config: cfg,
		db:     db,
		log:    logger.GetLogger("vpn.server"),
	}
	return server, nil
}

// Start запускает VPN сервер (или подключается к нему).
//
// Args:
//   - ctx: контекст для graceful shutdown
//
// Returns:
//   - error: ошибка запуска или nil при нормальной остановке
func (s *Server) Start(ctx context.Context) error {
	s.log.WithField("port", s.config.VPN.Port).Info("Starting VPN server integration")

	client, err := NewXrayClient(s.config.VPN.ApiAddress, s.config.VPN.ApiPort, "vless-inbound")
	if err != nil {
		return fmt.Errorf("failed to connect to xray api: %w", err)
	}
	s.xrayClient = client
	s.log.Info("Connected to Xray API")

	if s.db != nil {
		statsInterval := s.config.VPN.StatsInterval
		s.statsWorker = workers.NewStatsWorker(s.xrayClient, s.db, statsInterval)
		go s.statsWorker.Start(ctx)
		s.log.WithField("interval", statsInterval).Info("Traffic statistics worker started")
	}

	go s.monitorConnections(ctx)

	return nil
}

// Stop корректно останавливает работу с сервером.
//
// Args:
//   - ctx: контекст с таймаутом для graceful shutdown
//
// Returns:
//   - error: ошибка остановки или nil при успехе
func (s *Server) Stop(ctx context.Context) error {
	s.log.Info("Stopping VPN server integration...")

	if s.xrayClient != nil {
		return s.xrayClient.Close()
	}
	return nil
}

// AddUser добавляет пользователя в конфигурацию Xray/VLESS.
//
// Args:
//   - uuid: UUID пользователя
//
// Returns:
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
// Args:
//   - uuid: UUID пользователя для удаления
//
// Returns:
//   - error: ошибка удаления или nil при успехе
func (s *Server) RemoveUser(uuid string) error {
	if s.xrayClient == nil {
		return fmt.Errorf("xray client is not initialized")
	}
	return s.xrayClient.RemoveUser(context.Background(), uuid)
}

// GetUserStats возвращает статистику использования для конкретного пользователя.
//
// Args:
//   - uuid: UUID пользователя
//
// Returns:
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
	s.log.Info("Connection monitoring stopped")
}

// UserStats содержит статистику использования VPN для конкретного пользователя.
type UserStats struct {
	UUID          string // UUID пользователя
	BytesReceived uint64 // Байт получено (downlink)
	BytesSent     uint64 // Байт отправлено (uplink)
}
