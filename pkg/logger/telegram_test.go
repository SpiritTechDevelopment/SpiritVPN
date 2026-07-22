package logger

import (
	"testing"

	"github.com/sirupsen/logrus"
)

func TestNewTelegramHook(t *testing.T) {
	hook := NewTelegramHook("token", "chatID", "13", "api-server")

	if hook.BotToken != "token" {
		t.Errorf("Expected BotToken 'token', got '%s'", hook.BotToken)
	}

	if hook.ChatID != "chatID" {
		t.Errorf("Expected ChatID 'chatID', got '%s'", hook.ChatID)
	}

	if hook.ThreadID != "13" {
		t.Errorf("Expected ThreadID '13', got '%s'", hook.ThreadID)
	}

	if hook.Component != "api-server" {
		t.Errorf("Expected Component 'api-server', got '%s'", hook.Component)
	}
}

func TestTelegramHookLevels(t *testing.T) {
	hook := NewTelegramHook("token", "chatID", "13", "api-server")

	levels := hook.Levels()

	expectedLevels := []logrus.Level{
		logrus.PanicLevel,
		logrus.FatalLevel,
		logrus.ErrorLevel,
	}

	if len(levels) != len(expectedLevels) {
		t.Errorf("Expected %d levels, got %d", len(expectedLevels), len(levels))
	}

	for i, level := range levels {
		if level != expectedLevels[i] {
			t.Errorf("Expected level %s at index %d, got %s", expectedLevels[i], i, level)
		}
	}
}

func TestTelegramHookFormatMessage(t *testing.T) {
	tests := []struct {
		name      string
		component string
		wantEmoji string
	}{
		{"API Server", "api-server", "🔴 API"},
		{"VPN Server", "vpn-server", "🔐 VPN"},
		{"Infrastructure", "infrastructure", "⚙️ INFRA"},
		{"Database", "database", "💾 DB"},
		{"No component", "", "🚨"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hook := NewTelegramHook("token", "chatID", "13", tt.component)

			entry := &logrus.Entry{
				Level:   logrus.FatalLevel,
				Message: "Test error message",
				Data: logrus.Fields{
					"module":  "test.module",
					"user_id": 123,
				},
			}

			message := hook.formatMessage(entry)

			if tt.wantEmoji != "" && len(message) > 0 {
				t.Logf("Generated message: %s", message)
			}
		})
	}
}
