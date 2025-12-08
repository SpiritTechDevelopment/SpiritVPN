package bot

import (
	"context"
	"log"

	"github.com/RomanRyabinkin/SpiritVPN/internal/database"
	"github.com/RomanRyabinkin/SpiritVPN/pkg/config"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Bot представляет Telegram бота для управления VPN подписками.
// Обрабатывает команды пользователей, управляет подписками и выдает конфигурации VPN.
type Bot struct {
	config *config.Config
	db     *database.DB
	api    *tgbotapi.BotAPI
}

// NewBot создает новый экземпляр Telegram бота с заданной конфигурацией.
//
// Параметры:
//   - cfg: конфигурация приложения
//   - db: подключение к базе данных
//   - api: инстанс Telegram Bot API
//
// Возвращает:
//   - *Bot: инициализированный бот
func NewBot(cfg *config.Config, db *database.DB, api *tgbotapi.BotAPI) *Bot {
	return &Bot{
		config: cfg,
		db:     db,
		api:    api,
	}
}

// Start запускает бота и начинает обработку входящих обновлений.
// Блокирующая функция, которая работает до отмены контекста или ошибки.
//
// Параметры:
//   - ctx: контекст для graceful shutdown
//
// Возвращает:
//   - error: ошибка при работе бота или nil при нормальной остановке
func (b *Bot) Start(ctx context.Context) error {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := b.api.GetUpdatesChan(u)

	log.Println("Bot is listening for updates...")

	for {
		select {
		case <-ctx.Done():
			return nil
		case update := <-updates:
			b.handleUpdate(update)
		}
	}
}

// Stop останавливает бота и прекращает получение обновлений.
// Должен вызываться для корректного завершения работы.
func (b *Bot) Stop() {
	b.api.StopReceivingUpdates()
}

// handleUpdate обрабатывает входящее обновление от Telegram.
// Маршрутизирует обновление в соответствующий обработчик (сообщение или callback).
//
// Параметры:
//   - update: обновление от Telegram API
func (b *Bot) handleUpdate(update tgbotapi.Update) {
	if update.Message != nil {
		b.handleMessage(update.Message)
	} else if update.CallbackQuery != nil {
		b.handleCallbackQuery(update.CallbackQuery)
	}
}

// handleMessage обрабатывает текстовое сообщение от пользователя.
// Проверяет, является ли сообщение командой, и вызывает соответствующий обработчик.
//
// Параметры:
//   - message: сообщение от пользователя
func (b *Bot) handleMessage(message *tgbotapi.Message) {
	if !message.IsCommand() {
		return
	}

	switch message.Command() {
	case "start":
		b.handleStart(message)
	case "buy":
		b.handleBuy(message)
	case "myconfig":
		b.handleMyConfig(message)
	case "stats":
		b.handleStats(message)
	case "support":
		b.handleSupport(message)
	default:
		msg := tgbotapi.NewMessage(message.Chat.ID, "Неизвестная команда. Используйте /start")
		if _, err := b.api.Send(msg); err != nil {
			log.Printf("Failed to send message: %v", err)
		}
	}
}

// handleCallbackQuery обрабатывает callback запрос от inline кнопок.
// Вызывается когда пользователь нажимает на inline кнопку в сообщении.
//
// Параметры:
//   - query: callback запрос с данными нажатой кнопки
//
// TODO: Реализовать полную обработку callback'ов (выбор тарифа, подтверждение оплаты и т.д.)
func (b *Bot) handleCallbackQuery(query *tgbotapi.CallbackQuery) {
	// TODO: Обработка callback'ов от inline кнопок
	log.Printf("Callback: %s from user %d", query.Data, query.From.ID)
}

// handleStart обрабатывает команду /start.
// Отправляет приветственное сообщение с описанием сервиса и основное меню навигации.
// Точка входа для новых пользователей.
//
// Параметры:
//   - message: сообщение с командой /start
func (b *Bot) handleStart(message *tgbotapi.Message) {
	text := `👋 Добро пожаловать в SpiritVPN!

Быстрый, безопасный и надежный VPN-сервис.

🔹 Безлимитная скорость
🔹 Серверы по всему миру
🔹 Полная конфиденциальность
🔹 Простая настройка

Используйте кнопки ниже для навигации:`

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("💳 Купить подписку", "buy"),
			tgbotapi.NewInlineKeyboardButtonData("⚙️ Мой конфиг", "myconfig"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📊 Статистика", "stats"),
			tgbotapi.NewInlineKeyboardButtonData("💬 Поддержка", "support"),
		),
	)

	msg := tgbotapi.NewMessage(message.Chat.ID, text)
	msg.ReplyMarkup = keyboard
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("Failed to send message: %v", err)
	}
}

// handleBuy обрабатывает команду /buy.
// Показывает список доступных тарифных планов с описанием и ценами.
// Пользователь может выбрать план через inline кнопки.
//
// Параметры:
//   - message: сообщение с командой /buy
func (b *Bot) handleBuy(message *tgbotapi.Message) {
	text := `💳 Выберите тарифный план:

📦 Basic - 299₽/месяц
• 1 устройство
• 50 Мбит/с
• Базовая поддержка

⭐ Premium - 599₽/месяц
• 5 устройств
• Безлимитная скорость
• Приоритетная поддержка
• Все серверы

🎁 Premium Year - 5990₽/год (скидка 16%)
• Все преимущества Premium
• Экономия 1198₽`

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📦 Basic", "plan_basic"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⭐ Premium", "plan_premium"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🎁 Premium Year", "plan_premium_year"),
		),
	)

	msg := tgbotapi.NewMessage(message.Chat.ID, text)
	msg.ReplyMarkup = keyboard
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("Failed to send message: %v", err)
	}
}

// handleMyConfig обрабатывает команду /myconfig.
// Отправляет пользователю его VPN конфигурацию в виде файла и QR-кода.
// Требует активную подписку.
//
// Параметры:
//   - message: сообщение с командой /myconfig
//
// TODO: Реализовать получение конфига из БД, генерацию QR-кода
func (b *Bot) handleMyConfig(message *tgbotapi.Message) {
	// TODO: Получение конфига из БД
	text := "⚙️ Для получения конфигурации сначала оформите подписку /buy"
	msg := tgbotapi.NewMessage(message.Chat.ID, text)
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("Failed to send message: %v", err)
	}
}

// handleStats обрабатывает команду /stats.
// Показывает пользователю статистику использования VPN:
// израсходованный трафик, время подключения, оставшиеся дни подписки.
//
// Параметры:
//   - message: сообщение с командой /stats
//
// TODO: Реализовать получение статистики из БД
func (b *Bot) handleStats(message *tgbotapi.Message) {
	// TODO: Получение статистики из БД
	text := "📊 Статистика доступна только для активных пользователей"
	msg := tgbotapi.NewMessage(message.Chat.ID, text)
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("Failed to send message: %v", err)
	}
}

// handleSupport обрабатывает команду /support.
// Отправляет информацию о способах связи с технической поддержкой.
//
// Параметры:
//   - message: сообщение с командой /support
func (b *Bot) handleSupport(message *tgbotapi.Message) {
	text := `💬 Поддержка

Если у вас возникли вопросы или проблемы:

📧 Email: support@spiritvpn.com
💬 Telegram: @spiritvpn_support

⏰ Время ответа: до 24 часов

Пожалуйста, опишите вашу проблему максимально подробно.`

	msg := tgbotapi.NewMessage(message.Chat.ID, text)
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("Failed to send message: %v", err)
	}
}
