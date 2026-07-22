package logger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/sirupsen/logrus"
)

// TelegramHook - хук для отправки критических ошибок в Telegram.
// Поддерживает отправку сообщений в конкретный топик (thread) супергруппы.
type TelegramHook struct {
	BotToken  string
	ChatID    string
	ThreadID  string // ID топика (message_thread_id) для отправки в конкретный топик
	Component string // Название компонента/сервиса (api-server, vpn-server, infrastructure)
	client    *http.Client
}

// NewTelegramHook создает новый Telegram хук.
//
// Параметры:
//   - botToken: токен Telegram бота
//   - chatID: ID чата или супергруппы
//   - threadID: ID топика (опционально, передайте "" если не используется)
//   - component: название компонента (api-server, vpn-server, infrastructure)
//
// Для отправки в топик супергруппы:
//  1. Создайте супергруппу с включенными Topics
//  2. Получите message_thread_id топика (можно через GetUpdates API)
//  3. Передайте его как threadID
//
// Примеры использования:
//
//	hook := NewTelegramHook(token, chatID, "13", "api-server")
//	hook := NewTelegramHook(token, chatID, "13", "infrastructure")
func NewTelegramHook(botToken, chatID, threadID, component string) *TelegramHook {
	return &TelegramHook{
		BotToken:  botToken,
		ChatID:    chatID,
		ThreadID:  threadID,
		Component: component,
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// Levels возвращает уровни логирования для отправки в Telegram
func (hook *TelegramHook) Levels() []logrus.Level {
	return []logrus.Level{
		logrus.PanicLevel,
		logrus.FatalLevel,
		logrus.ErrorLevel,
	}
}

// Fire отправляет сообщение в Telegram
func (hook *TelegramHook) Fire(entry *logrus.Entry) error {
	if entry.Level != logrus.FatalLevel && entry.Level != logrus.PanicLevel {
		return nil
	}

	message := hook.formatMessage(entry)

	if len(message) > 4000 {
		message = message[:4000] + "..."
	}

	return hook.sendMessage(message)
}

// formatMessage форматирует сообщение для Telegram
func (hook *TelegramHook) formatMessage(entry *logrus.Entry) string {
	var buf bytes.Buffer

	// Определяем префикс по типу компонента
	prefix := "ERROR"
	switch hook.Component {
	case "api-server":
		prefix = "API"
	case "vpn-server":
		prefix = "VPN"
	case "infrastructure":
		prefix = "INFRA"
	case "database":
		prefix = "DB"
	}

	// Заголовок с компонентом и уровнем
	if hook.Component != "" {
		fmt.Fprintf(&buf, "<b>%s %s</b>\n\n", prefix, entry.Level.String())
	} else {
		fmt.Fprintf(&buf, "<b>%s</b>\n\n", entry.Level.String())
	}

	fmt.Fprintf(&buf, "<b>Time:</b> %s\n", entry.Time.Format(time.RFC3339))

	if module, ok := entry.Data["module"].(string); ok {
		fmt.Fprintf(&buf, "<b>Module:</b> %s\n", module)
	}

	if userID, ok := entry.Data["user_id"]; ok {
		fmt.Fprintf(&buf, "<b>User ID:</b> %v\n", userID)
	}

	fmt.Fprintf(&buf, "\n<b>Message:</b>\n%s\n", entry.Message)

	if len(entry.Data) > 0 {
		buf.WriteString("\n<b>Context:</b>\n")
		for key, value := range entry.Data {
			if key != "module" && key != "user_id" {
				fmt.Fprintf(&buf, "• %s: %v\n", key, value)
			}
		}
	}

	return buf.String()
}

// sendMessage отправляет сообщение в Telegram.
// Если указан ThreadID, сообщение будет отправлено в конкретный топик супергруппы.
func (hook *TelegramHook) sendMessage(text string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", hook.BotToken)

	payload := map[string]interface{}{
		"chat_id":    hook.ChatID,
		"text":       text,
		"parse_mode": "HTML",
	}

	if hook.ThreadID != "" {
		payload["message_thread_id"] = hook.ThreadID
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	resp, err := hook.client.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram api returned status %d", resp.StatusCode)
	}

	return nil
}
