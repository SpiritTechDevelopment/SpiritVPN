package api

import "github.com/gin-gonic/gin"

const (
	// PublicAPIV1Prefix задаёт базовый путь публичного API версии 1.
	PublicAPIV1Prefix = "/api/v1"
	// InternalAPIV1Prefix задаёт базовый путь внутреннего service-to-service API версии 1.
	InternalAPIV1Prefix = "/internal/v1"

	// TestAccessPath задаёт относительный путь выдачи тестового VPN-доступа.
	TestAccessPath = "/vpn/test-access"
	// TestAccessRoute содержит полный путь выдачи тестового VPN-доступа.
	TestAccessRoute = InternalAPIV1Prefix + TestAccessPath
)

// registerInternalRoutes регистрирует внутренние service-to-service маршруты
func registerInternalRoutes(router gin.IRouter, token string, accessIssuer testAccessIssuer) {
	internal := router.Group(InternalAPIV1Prefix, internalTokenAuth(token))
	registerAccessRoutes(internal, accessIssuer)
}

// registerAccessRoutes регистрирует маршруты модуля выдачи VPN-доступов.
func registerAccessRoutes(router gin.IRoutes, issuer testAccessIssuer) {
	router.POST(TestAccessPath, issueTestAccessHandler(issuer))
}
