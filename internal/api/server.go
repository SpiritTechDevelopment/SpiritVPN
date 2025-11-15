package api

import (
	"github.com/RomanRyabinkin/SpiritVPN/internal/database"
	"github.com/RomanRyabinkin/SpiritVPN/pkg/config"
	"github.com/gin-gonic/gin"
)

// Server представляет API сервер
type Server struct {
	config *config.Config
	db     *database.DB
	router *gin.Engine
}

// NewServer создает новый API сервер
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

// Router возвращает HTTP router
func (s *Server) Router() *gin.Engine {
	return s.router
}

// setupRoutes настраивает маршруты API
func (s *Server) setupRoutes() {
	// Health check
	s.router.GET("/health", s.healthCheck)

	// API v1
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

// healthCheck проверка работоспособности
func (s *Server) healthCheck(c *gin.Context) {
	c.JSON(200, gin.H{
		"status":  "ok",
		"service": "SpiritVPN API",
	})
}

// register регистрация нового пользователя
func (s *Server) register(c *gin.Context) {
	// TODO: Реализовать регистрацию
	c.JSON(501, gin.H{
		"success": false,
		"error":   "Not implemented",
	})
}

// login вход пользователя
func (s *Server) login(c *gin.Context) {
	// TODO: Реализовать логин
	c.JSON(501, gin.H{
		"success": false,
		"error":   "Not implemented",
	})
}

// refreshToken обновление токена
func (s *Server) refreshToken(c *gin.Context) {
	// TODO: Реализовать обновление токена
	c.JSON(501, gin.H{
		"success": false,
		"error":   "Not implemented",
	})
}
