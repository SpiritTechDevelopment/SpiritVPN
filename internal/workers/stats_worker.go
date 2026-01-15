package workers

import (
	"context"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/database"
	"github.com/RomanRyabinkin/SpiritVPN/pkg/logger"
	"github.com/sirupsen/logrus"
)

// XrayStatsClient определяет интерфейс для получения статистики из Xray API.
type XrayStatsClient interface {
	// GetStats возвращает статистику трафика для пользователя.
	//
	// Args:
	//   - ctx: контекст для отмены операции
	//   - email: идентификатор пользователя в Xray
	//
	// Returns:
	//   - int64: количество полученных байт (downlink)
	//   - int64: количество отправленных байт (uplink)
	//   - error: ошибка получения или nil
	GetStats(ctx context.Context, email string) (int64, int64, error)
}

// StatsWorker представляет фоновый процесс для периодического сбора статистики трафика.
//
// Воркер периодически опрашивает Xray API для получения данных об использовании трафика
// всеми активными пользователями и сохраняет агрегированную статистику в базу данных.
//
// Fields:
//   - xrayClient: клиент для взаимодействия с Xray gRPC API
//   - db: подключение к базе данных для сохранения статистики
//   - interval: интервал между циклами сбора статистики
//   - log: логгер для записи событий и ошибок воркера
//
// Notes:
//   - Воркер запускается в отдельной горутине и работает до получения сигнала отмены через context
//   - При ошибках обработки отдельных пользователей воркер продолжает работу с остальными
//   - Статистика сохраняется с точностью до дня (truncated to 24h)
type StatsWorker struct {
	xrayClient XrayStatsClient
	db         *database.DB
	interval   time.Duration
	log        *logrus.Entry
}

// NewStatsWorker создает и инициализирует новый воркер для сбора статистики трафика.
//
// Создает экземпляр StatsWorker с заданными параметрами и настроенным логгером.
// Воркер готов к запуску через метод Start().
//
// Args:
//   - xrayClient: клиент реализующий интерфейс XrayStatsClient
//   - db: подключение к базе данных PostgreSQL через GORM
//   - interval: интервал между циклами сбора статистики (рекомендуется 5m)
//
// Returns:
//   - *StatsWorker: готовый к использованию экземпляр воркера
//
// Example:
//
//	worker := NewStatsWorker(xrayClient, db, 5*time.Minute)
//	go worker.Start(ctx)
func NewStatsWorker(xrayClient XrayStatsClient, db *database.DB, interval time.Duration) *StatsWorker {
	return &StatsWorker{
		xrayClient: xrayClient,
		db:         db,
		interval:   interval,
		log:        logger.GetLogger("vpn.stats_worker"),
	}
}

// Start запускает фоновый процесс периодического сбора статистики трафика.
//
// Метод блокирующий и должен запускаться в отдельной горутине.
// Использует ticker для периодического вызова collectStats с заданным интервалом.
// Корректно завершает работу при отмене контекста.
//
// Args:
//   - ctx: контекст для управления жизненным циклом воркера и отмены операций
//
// Notes:
//   - Метод блокирует выполнение до получения сигнала отмены через ctx.Done()
//   - Ticker автоматически останавливается при завершении работы (defer ticker.Stop())
//   - Первый сбор статистики произойдет через interval после запуска
//   - При ошибках сбора статистики воркер продолжает работу
//
// Example:
//
//	ctx, cancel := context.WithCancel(context.Background())
//	defer cancel()
//	go worker.Start(ctx)
func (w *StatsWorker) Start(ctx context.Context) {
	w.log.Info("Starting traffic statistics worker")

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.log.Info("Stopping traffic statistics worker")
			return
		case <-ticker.C:
			w.collectStats(ctx)
		}
	}
}

// collectStats выполняет один цикл сбора статистики трафика для всех активных пользователей.
//
// Процесс сбора:
//  1. Получает все активные подписки из базы данных
//  2. Для каждой подписки получает связанные VPN конфигурации
//  3. Для каждой конфигурации запрашивает статистику из Xray API
//  4. Сохраняет полученные данные в таблицу traffic_stats
//
// Args:
//   - ctx: контекст для отмены операций и передачи в Xray API вызовы
//
// Notes:
//   - Метод устойчив к ошибкам: при проблемах с отдельным пользователем продолжает обработку остальных
//   - Использует email пользователя для идентификации в Xray, если email пустой - использует UUID
//   - Статистика агрегируется по дням (дата truncated до 00:00:00 UTC)
//   - Создает отдельные экземпляры репозиториев для каждого вызова (thread-safe)
//   - Логирует предупреждения для ошибок отдельных пользователей и ошибки для критичных сбоев
func (w *StatsWorker) collectStats(ctx context.Context) {
	w.log.Debug("Collecting traffic statistics")

	configRepo := database.NewVPNConfigRepository(w.db)
	trafficRepo := database.NewTrafficStatsRepository(w.db)

	subscriptionRepo := database.NewSubscriptionRepository(w.db)
	activeSubscriptions, err := subscriptionRepo.GetAllActive()
	if err != nil {
		w.log.WithError(err).Error("Failed to get active subscriptions")
		return
	}

	for _, subscription := range activeSubscriptions {
		configs, err := configRepo.GetBySubscriptionID(subscription.ID)
		if err != nil {
			w.log.WithError(err).WithField("subscription_id", subscription.ID).Warn("Failed to get configs for subscription")
			continue
		}

		for _, config := range configs {
			userRepo := database.NewUserRepository(w.db)
			user, err := userRepo.GetByID(config.UserID)
			if err != nil || user == nil {
				w.log.WithError(err).WithField("user_id", config.UserID).Warn("Failed to get user")
				continue
			}

			email := user.Email
			if email == "" {
				email = config.UUID
			}

			received, sent, err := w.xrayClient.GetStats(ctx, email)
			if err != nil {
				w.log.WithFields(logrus.Fields{
					"user_id": config.UserID,
					"email":   email,
				}).WithError(err).Warn("Failed to get user stats")
				continue
			}

			stat := &database.TrafficStat{
				UserID:        config.UserID,
				ConfigID:      config.ID,
				Date:          time.Now().UTC().Truncate(24 * time.Hour),
				BytesReceived: received,
				BytesSent:     sent,
			}

			if err := trafficRepo.Create(stat); err != nil {
				w.log.WithFields(logrus.Fields{
					"user_id": config.UserID,
					"email":   email,
				}).WithError(err).Error("Failed to save traffic stats")
				continue
			}

			w.log.WithFields(logrus.Fields{
				"user_id":  config.UserID,
				"email":    email,
				"received": received,
				"sent":     sent,
			}).Debug("Traffic stats collected")
		}
	}
}
