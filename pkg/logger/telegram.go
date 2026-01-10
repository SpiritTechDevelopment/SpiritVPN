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

type TelegramHook struct {
	BotToken string
	ChatID   string
	client   *http.Client
}

// NewTelegramHook создает новый Telegram хук
func NewTelegramHook(botToken, chatID string) *TelegramHook {
	return &TelegramHook{
		BotToken: botToken,
		ChatID:   chatID,
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

	buf.WriteString(fmt.Sprintf("🚨 <b>%s</b>\n\n", entry.Level.String()))
	buf.WriteString(fmt.Sprintf("<b>Time:</b> %s\n", entry.Time.Format(time.RFC3339)))

	if module, ok := entry.Data["module"].(string); ok {
		buf.WriteString(fmt.Sprintf("<b>Module:</b> %s\n", module))
	}

	if userID, ok := entry.Data["user_id"]; ok {
		buf.WriteString(fmt.Sprintf("<b>User ID:</b> %v\n", userID))
	}

	buf.WriteString(fmt.Sprintf("\n<b>Message:</b>\n%s\n", entry.Message))

	if len(entry.Data) > 0 {
		buf.WriteString("\n<b>Context:</b>\n")
		for key, value := range entry.Data {
			if key != "module" && key != "user_id" {
				buf.WriteString(fmt.Sprintf("• %s: %v\n", key, value))
			}
		}
	}

	return buf.String()
}

// sendMessage отправляет сообщение в Telegram
func (hook *TelegramHook) sendMessage(text string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", hook.BotToken)

	payload := map[string]interface{}{
		"chat_id":    hook.ChatID,
		"text":       text,
		"parse_mode": "HTML",
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	resp, err := hook.client.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram api returned status %d", resp.StatusCode)
	}

	return nil
}
