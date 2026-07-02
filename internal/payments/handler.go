package payments

import (
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
)

// SetupRoutes регистрирует HTTP-маршруты модуля платежей в предоставленном маршрутизаторе Gin.
// Функция инициализирует эндпоинты для инициации транзакций клиентами и обработки 
// асинхронных уведомлений (вебхуков) от внешних платежных провайдеров.
//
// Параметры:
//   - router: экземпляр маршрутизатора Gin, к которому будут привязаны эндпоинты.
//   - svc: экземпляр сервиса бизнес-логики платежей.
func SetupRoutes(router *gin.Engine, svc *Service) {
	group := router.Group("/api/v1/payments")
	{
		// POST /api/v1/payments/create
		// Предназначен для создания нового платежного инвойса.
		// Принимает JSON с идентификатором пользователя и суммой платежа.
		// Возвращает сгенерированный URL-адрес для перенаправления пользователя на страницу оплаты.
		group.POST("/create", func(c *gin.Context) {
			var req struct {
				UserID uint    `json:"user_id"`
				Amount float64 `json:"amount"`
			}

			if err := c.BindJSON(&req); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request payload"})
				return
			}

			url, err := svc.GeneratePaymentLink(c.Request.Context(), req.UserID, req.Amount, "RUB")
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}

			c.JSON(http.StatusOK, gin.H{"payment_url": url})
		})

		// POST /api/v1/payments/webhook/cryptomus
		// Эндпоинт для приема асинхронных серверных уведомлений (Server-to-Server) от Cryptomus.
		group.POST("/webhook/cryptomus", func(c *gin.Context) {
			// Чтение сырого тела запроса необходимо для корректного вычисления хеша подписи
			rawBody, err := io.ReadAll(c.Request.Body)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read request body"})
				return
			}

			// Извлечение криптографической подписи из HTTP header'ов
			signature := c.GetHeader("sign")

			// Передача сырых данных в слой сервиса для верификации и обновления состояния БД
			err = svc.ProcessWebhook(c.Request.Context(), rawBody, signature)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Webhook processing failed"})
				return
			}

			c.String(http.StatusOK, "OK")
		})
	}
}