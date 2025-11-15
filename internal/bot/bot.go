package bot

import (
	"context"
	"log"

	"github.com/RomanRyabinkin/SpiritVPN/internal/database"
	"github.com/RomanRyabinkin/SpiritVPN/pkg/config"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Bot представляет Telegram бота
type Bot struct {
	config *config.Config
	db     *database.DB
	api    *tgbotapi.BotAPI
}

// NewBot создает нового бота
func NewBot(cfg *config.Config, db *database.DB, api *tgbotapi.BotAPI) *Bot {
	return &Bot{
		config: cfg,
		db:     db,
		api:    api,
	}
}

// Start запускает бота
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

// Stop останавливает бота
func (b *Bot) Stop() {
	b.api.StopReceivingUpdates()
}

// handleUpdate обрабатывает обновление
func (b *Bot) handleUpdate(update tgbotapi.Update) {
	if update.Message != nil {
		b.handleMessage(update.Message)
	} else if update.CallbackQuery != nil {
		b.handleCallbackQuery(update.CallbackQuery)
	}
}

// handleMessage обрабатывает текстовое сообщение
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
		b.api.Send(msg)
	}
}

// handleCallbackQuery обрабатывает callback от inline кнопок
func (b *Bot) handleCallbackQuery(query *tgbotapi.CallbackQuery) {
	// TODO: Обработка callback'ов от inline кнопок
	log.Printf("Callback: %s from user %d", query.Data, query.From.ID)
}

// handleStart обрабатывает команду /start
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
	b.api.Send(msg)
}

// handleBuy обрабатывает команду /buy
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
	b.api.Send(msg)
}

// handleMyConfig обрабатывает команду /myconfig
func (b *Bot) handleMyConfig(message *tgbotapi.Message) {
	// TODO: Получение конфига из БД
	text := "⚙️ Для получения конфигурации сначала оформите подписку /buy"
	msg := tgbotapi.NewMessage(message.Chat.ID, text)
	b.api.Send(msg)
}

// handleStats обрабатывает команду /stats
func (b *Bot) handleStats(message *tgbotapi.Message) {
	// TODO: Получение статистики из БД
	text := "📊 Статистика доступна только для активных пользователей"
	msg := tgbotapi.NewMessage(message.Chat.ID, text)
	b.api.Send(msg)
}

// handleSupport обрабатывает команду /support
func (b *Bot) handleSupport(message *tgbotapi.Message) {
	text := `💬 Поддержка

Если у вас возникли вопросы или проблемы:

📧 Email: support@spiritvpn.com
💬 Telegram: @spiritvpn_support

⏰ Время ответа: до 24 часов

Пожалуйста, опишите вашу проблему максимально подробно.`

	msg := tgbotapi.NewMessage(message.Chat.ID, text)
	b.api.Send(msg)
}
