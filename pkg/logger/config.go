package logger

import (
	"os"
	"strconv"
	"time"
)

// Config содержит настройки логгера.
type Config struct {
	// Level - уровень логирования (debug, info, warning, error, fatal, panic)
	Level string

	// LogDir - директория для хранения лог-файлов
	LogDir string

	// ConsoleOutput - выводить ли логи в консоль
	ConsoleOutput bool

	// FileOutput - записывать ли логи в файл
	FileOutput bool

	// ColoredOutput - использовать ли цветной вывод (только для консоли)
	ColoredOutput bool

	// ErrorLogFile - создавать ли отдельный файл для ошибок
	ErrorLogFile bool

	// Enabled - включить/выключить логирование
	Enabled bool

	// TimestampFormat - формат временной метки
	TimestampFormat string

	// MaxFileSize - максимальный размер лог-файла в мегабайтах (для ротации)
	MaxFileSize int

	// MaxBackups - максимальное количество старых лог-файлов
	MaxBackups int

	// MaxAge - максимальное количество дней хранения лог-файлов
	MaxAge int

	// TelegramBotToken - токен Telegram бота для отправки критических ошибок
	TelegramBotToken string

	// TelegramChatID - ID чата для отправки уведомлений
	TelegramChatID string
}

// DefaultConfig возвращает конфигурацию по умолчанию.
func DefaultConfig() *Config {
	return &Config{
		Level:           "info",
		LogDir:          "./logs",
		ConsoleOutput:   true,
		FileOutput:      true,
		ColoredOutput:   true,
		ErrorLogFile:    true,
		Enabled:         true,
		TimestampFormat: time.RFC3339, // "2006-01-02T15:04:05Z07:00"
		MaxFileSize:     10,           // 10 MB
		MaxBackups:      5,
		MaxAge:          30, // 30 дней
	}
}

// LoadFromEnv загружает конфигурацию логгера из переменных окружения.
// Возвращает конфигурацию с установленными значениями.
func LoadFromEnv() *Config {
	cfg := DefaultConfig()

	cfg.Level = getEnv("LOG_LEVEL", cfg.Level)
	cfg.LogDir = getEnv("LOG_DIR", cfg.LogDir)
	cfg.ConsoleOutput = getEnvAsBool("LOG_CONSOLE", cfg.ConsoleOutput)
	cfg.FileOutput = getEnvAsBool("LOG_FILE", cfg.FileOutput)
	cfg.ColoredOutput = getEnvAsBool("LOG_COLORED", cfg.ColoredOutput)
	cfg.ErrorLogFile = getEnvAsBool("LOG_ERROR_FILE", cfg.ErrorLogFile)
	cfg.Enabled = getEnvAsBool("LOG_ENABLED", cfg.Enabled)
	cfg.MaxFileSize = getEnvAsInt("LOG_MAX_FILE_SIZE", cfg.MaxFileSize)
	cfg.MaxBackups = getEnvAsInt("LOG_MAX_BACKUPS", cfg.MaxBackups)
	cfg.MaxAge = getEnvAsInt("LOG_MAX_AGE", cfg.MaxAge)
	cfg.TelegramBotToken = getEnv("LOG_TELEGRAM_BOT_TOKEN", cfg.TelegramBotToken)
	cfg.TelegramChatID = getEnv("LOG_TELEGRAM_CHAT_ID", cfg.TelegramChatID)

	return cfg
}

// getEnv получает значение переменной окружения или возвращает значение по умолчанию.
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvAsBool получает значение переменной окружения как boolean.
func getEnvAsBool(key string, defaultValue bool) bool {
	valueStr := os.Getenv(key)
	if valueStr == "" {
		return defaultValue
	}

	value, err := strconv.ParseBool(valueStr)
	if err != nil {
		return defaultValue
	}
	return value
}

// getEnvAsInt получает значение переменной окружения как целое число.
func getEnvAsInt(key string, defaultValue int) int {
	valueStr := os.Getenv(key)
	if valueStr == "" {
		return defaultValue
	}

	value, err := strconv.Atoi(valueStr)
	if err != nil {
		return defaultValue
	}
	return value
}
