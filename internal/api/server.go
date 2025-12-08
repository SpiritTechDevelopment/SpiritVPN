package api

import (
	"github.com/RomanRyabinkin/SpiritVPN/internal/database"
	"github.com/RomanRyabinkin/SpiritVPN/pkg/config"
	"github.com/gin-gonic/gin"
)

// Server представляет REST API сервер для управления VPN сервисом.
// Обрабатывает HTTP запросы для аутентификации, управления пользователями,
// подписками, конфигурациями VPN и платежами.
type Server struct {
	config *config.Config
	db     *database.DB
	router *gin.Engine
}

// NewServer создает и инициализирует новый API сервер.
// Настраивает Gin router, регистрирует маршруты и middleware.
//
// Параметры:
//   - cfg: конфигурация приложения
//   - db: подключение к базе данных
//
// Возвращает:
//   - *Server: готовый к запуску API сервер
func NewServer(cfg *config.Config, db *database.DB) *Server {
	if cfg.API.Mode == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.Default()

	server := &Server{
		config: cfg,
		db:     db,
		router: router,
	}

	server.setupRoutes()

	return server
}

// Router возвращает настроенный HTTP router (Gin Engine).
// Используется для запуска HTTP сервера.
//
// Возвращает:
//   - *gin.Engine: Gin router с зарегистрированными маршрутами
func (s *Server) Router() *gin.Engine {
	return s.router
}

// setupRoutes регистрирует все HTTP маршруты и группы эндпоинтов.
// Включает health check, аутентификацию и API v1 endpoints.
// Вызывается автоматически при создании сервера.
func (s *Server) setupRoutes() {
	s.router.GET("/health", s.healthCheck)

	v1 := s.router.Group("/api/v1")
	{
		// Аутентификация
		auth := v1.Group("/auth")
		{
			auth.POST("/register", s.register)
			auth.POST("/login", s.login)
			auth.POST("/refresh", s.refreshToken)
		}

		// Пользователи (требуют аутентификации)
		// users := v1.Group("/users")
		// users.Use(s.authMiddleware())
		// {
		// 	users.GET("/:id", s.getUser)
		// 	users.PUT("/:id", s.updateUser)
		// }

		// TODO: Добавить остальные эндпоинты
		// - /subscriptions
		// - /configs
		// - /payments
		// - /stats
		// - /servers
	}
}

// healthCheck обрабатывает GET /health эндпоинт.
// Возвращает статус работоспособности API сервера.
// Используется для мониторинга и load balancer health checks.
//
// Параметры:
//   - c: Gin контекст запроса
func (s *Server) healthCheck(c *gin.Context) {
	c.JSON(200, gin.H{
		"status":  "ok",
		"service": "SpiritVPN API",
	})
}

// register обрабатывает POST /api/v1/auth/register эндпоинт.
// Регистрирует нового пользователя в системе и возвращает JWT токены.
//
// Параметры:
//   - c: Gin контекст с данными регистрации (telegram_id, username, email)
//
// TODO: Реализовать полную регистрацию с валидацией и генерацией JWT
func (s *Server) register(c *gin.Context) {
	// TODO: Реализовать регистрацию
	c.JSON(501, gin.H{
		"success": false,
		"error":   "Not implemented",
	})
}

// login обрабатывает POST /api/v1/auth/login эндпоинт.
// Аутентифицирует пользователя и возвращает JWT токены.
//
// Параметры:
//   - c: Gin контекст с данными входа (telegram_id)
//
// TODO: Реализовать аутентификацию и генерацию JWT
func (s *Server) login(c *gin.Context) {
	// TODO: Реализовать логин
	c.JSON(501, gin.H{
		"success": false,
		"error":   "Not implemented",
	})
}

// refreshToken обрабатывает POST /api/v1/auth/refresh эндпоинт.
// Обновляет истекший access token используя refresh token.
//
// Параметры:
//   - c: Gin контекст с refresh_token в теле запроса
//
// TODO: Реализовать обновление токенов
func (s *Server) refreshToken(c *gin.Context) {
	// TODO: Реализовать обновление токена
	c.JSON(501, gin.H{
		"success": false,
		"error":   "Not implemented",
	})
}
