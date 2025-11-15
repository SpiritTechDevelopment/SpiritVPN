package config

import (
	"fmt"
	"os"

	"github.com/joho/godotenv"
)

// Config содержит всю конфигурацию приложения
type Config struct {
	API      APIConfig
	Database DatabaseConfig
	Redis    RedisConfig
	VPN      VPNConfig
	Telegram TelegramConfig
	Payment  PaymentConfig
}

// APIConfig конфигурация API сервера
type APIConfig struct {
	Address   string
	Mode      string // "debug" or "production"
	JWTSecret string
}

// DatabaseConfig конфигурация базы данных
type DatabaseConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	Name     string
}

// RedisConfig конфигурация Redis
type RedisConfig struct {
	Host     string
	Port     int
	Password string
	DB       int
}

// VPNConfig конфигурация VPN сервера
type VPNConfig struct {
	Port       int
	Subnet     string
	Interface  string
	PrivateKey string
}

// TelegramConfig конфигурация Telegram бота
type TelegramConfig struct {
	BotToken string
	Debug    bool
}

// PaymentConfig конфигурация платежных систем
type PaymentConfig struct {
	YooKassaShopID    string
	YooKassaSecretKey string
}

// Load загружает конфигурацию из переменных окружения
func Load() (*Config, error) {
	// Попытка загрузить .env файл (игнорируем ошибку если файл не найден)
	_ = godotenv.Load("configs/.env")

	cfg := &Config{
		API: APIConfig{
			Address:   getEnv("API_ADDRESS", ":8080"),
			Mode:      getEnv("API_MODE", "debug"),
			JWTSecret: getEnv("JWT_SECRET", "your-secret-key-change-in-production"),
		},
		Database: DatabaseConfig{
			Host:     getEnv("DB_HOST", "localhost"),
			Port:     getEnvAsInt("DB_PORT", 5432),
			User:     getEnv("DB_USER", "spiritdb"),
			Password: getEnv("DB_PASSWORD", ""),
			Name:     getEnv("DB_NAME", "spiritdb"),
		},
		Redis: RedisConfig{
			Host:     getEnv("REDIS_HOST", "localhost"),
			Port:     getEnvAsInt("REDIS_PORT", 6379),
			Password: getEnv("REDIS_PASSWORD", ""),
			DB:       getEnvAsInt("REDIS_DB", 0),
		},
		VPN: VPNConfig{
			Port:       getEnvAsInt("VPN_PORT", 51820),
			Subnet:     getEnv("VPN_SUBNET", "10.8.0.0/24"),
			Interface:  getEnv("VPN_INTERFACE", "wg0"),
			PrivateKey: getEnv("VPN_PRIVATE_KEY", ""),
		},
		Telegram: TelegramConfig{
			BotToken: getEnv("TELEGRAM_BOT_TOKEN", ""),
			Debug:    getEnv("TELEGRAM_DEBUG", "false") == "true",
		},
		Payment: PaymentConfig{
			YooKassaShopID:    getEnv("YOOKASSA_SHOP_ID", ""),
			YooKassaSecretKey: getEnv("YOOKASSA_SECRET_KEY", ""),
		},
	}

	// Валидация обязательных параметров
	if cfg.Telegram.BotToken == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
	}

	if cfg.Database.Password == "" {
		fmt.Println("Warning: DB_PASSWORD is empty")
	}

	return cfg, nil
}

// getEnv получает значение переменной окружения или возвращает default
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvAsInt получает значение как int или возвращает default
func getEnvAsInt(key string, defaultValue int) int {
	valueStr := os.Getenv(key)
	if valueStr == "" {
		return defaultValue
	}
	var value int
	fmt.Sscanf(valueStr, "%d", &value)
	return value
}
