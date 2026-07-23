package api

import (
	"github.com/RomanRyabinkin/SpiritVPN/internal/api/handlers"
	"github.com/RomanRyabinkin/SpiritVPN/internal/database"
	"github.com/RomanRyabinkin/SpiritVPN/internal/payments"
	"github.com/RomanRyabinkin/SpiritVPN/pkg/config"
	"github.com/gin-gonic/gin"
)

// Server объединяет HTTP-маршруты API и их прикладные зависимости.
type Server struct {
	config         *config.Config
	db             *database.DB
	router         *gin.Engine
	paymentService *payments.Service
	accessService  testAccessIssuer
}

// NewServer создаёт HTTP-сервер, настраивает режим Gin и регистрирует маршруты API.
func NewServer(cfg *config.Config, db *database.DB, paymentService *payments.Service, accessService testAccessIssuer) *Server {
	if cfg.API.Mode == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.Default()

	server := &Server{
		config:         cfg,
		db:             db,
		router:         router,
		paymentService: paymentService,
		accessService:  accessService,
	}

	server.setupRoutes()

	return server
}

// Router возвращает настроенный HTTP-обработчик Gin.
func (s *Server) Router() *gin.Engine {
	return s.router
}

func (s *Server) setupRoutes() {
	s.router.GET("/health", handlers.HealthCheck)
	s.router.GET("/health/advanced", handlers.HealthCheckAdvanced(s.db))

	v1 := s.router.Group(PublicAPIV1Prefix)
	{
		auth := v1.Group("/auth")
		{
			auth.POST("/register", s.register)
			auth.POST("/login", s.login)
			auth.POST("/refresh", s.refreshToken)
		}

	}

	payments.SetupRoutes(s.router, s.paymentService)
	registerInternalRoutes(s.router, s.config.API.InternalToken, s.accessService)
}

func (s *Server) register(c *gin.Context) {
	c.JSON(501, gin.H{"success": false, "error": "Not implemented"})
}

func (s *Server) login(c *gin.Context) {
	c.JSON(501, gin.H{"success": false, "error": "Not implemented"})
}

func (s *Server) refreshToken(c *gin.Context) {
	c.JSON(501, gin.H{"success": false, "error": "Not implemented"})
}
