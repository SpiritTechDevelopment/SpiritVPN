package config

import (
	"fmt"
	"os"

	"github.com/joho/godotenv"
)

// Config содержит всю конфигурацию приложения SpiritVPN.
// Загружается из переменных окружения или .env файла.
type Config struct {
	API      APIConfig      // Настройки REST API сервера
	Database DatabaseConfig // Настройки PostgreSQL
	Redis    RedisConfig    // Настройки Redis кеша
	VPN      VPNConfig      // Настройки VPN сервера
	Telegram TelegramConfig // Настройки Telegram бота
	Payment  PaymentConfig  // Настройки платежных систем
}

// APIConfig содержит конфигурацию REST API сервера.
type APIConfig struct {
	Address   string // Адрес для прослушивания (например, ":8080")
	Mode      string // Режим работы: "debug" или "production"
	JWTSecret string // Секретный ключ для подписи JWT токенов
}

// DatabaseConfig содержит параметры подключения к PostgreSQL.
type DatabaseConfig struct {
	Host     string // Хост БД (например, "localhost")
	Port     int    // Порт БД (обычно 5432)
	User     string // Имя пользователя БД
	Password string // Пароль пользователя БД
	Name     string // Имя базы данных
}

// RedisConfig содержит параметры подключения к Redis.
// Используется для кеширования и хранения сессий.
type RedisConfig struct {
	Host     string // Хост Redis (например, "localhost")
	Port     int    // Порт Redis (обычно 6379)
	Password string // Пароль Redis (пустой если не установлен)
	DB       int    // Номер базы данных Redis (0-15)
}

// VPNConfig содержит конфигурацию VLESS сервера.
type VPNConfig struct {
	Port       int    // Порт (обычно 443 для VLESS+Reality)
	ServerName string // SNI домен (например, google.com для Reality)
	PrivateKey string // Приватный ключ сервера (X25519)
	ShortIds   string // ShortIds для Reality (через запятую)
}

// TelegramConfig содержит конфигурацию Telegram бота.
type TelegramConfig struct {
	BotToken string // Токен бота от @BotFather
	Debug    bool   // Включить отладочные логи
}

// PaymentConfig содержит конфигурацию интеграции с платежными системами.
type PaymentConfig struct {
	YooKassaShopID    string // Shop ID от ЮКасса
	YooKassaSecretKey string // Секретный ключ от ЮКасса
}

// Load загружает конфигурацию приложения из переменных окружения.
// Сначала пытается загрузить configs/.env файл, затем читает системные переменные.
// Устанавливает значения по умолчанию для необязательных параметров.
//
// Возвращает:
//   - *Config: загруженная конфигурация
//   - error: ошибка валидации (например, отсутствует TELEGRAM_BOT_TOKEN)
//
// Пример:
//
//	cfg, err := config.Load()
//	if err != nil {
//	    log.Fatal(err)
//	}
func Load() (*Config, error) {
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
			Port:       getEnvAsInt("VPN_PORT", 443),
			ServerName: getEnv("VPN_SERVER_NAME", "google.com"),
			PrivateKey: getEnv("VPN_PRIVATE_KEY", ""),
			ShortIds:   getEnv("VPN_SHORT_IDS", ""),
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

// getEnv получает значение переменной окружения или возвращает значение по умолчанию.
// Вспомогательная функция для загрузки конфигурации.
//
// Параметры:
//   - key: имя переменной окружения
//   - defaultValue: значение по умолчанию, если переменная не установлена
//
// Возвращает:
//   - string: значение переменной или defaultValue
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvAsInt получает значение переменной окружения как целое число.
// Вспомогательная функция для загрузки числовых конфигураций (порты, таймауты).
//
// Параметры:
//   - key: имя переменной окружения
//   - defaultValue: значение по умолчанию, если переменная не установлена или не является числом
//
// Возвращает:
//   - int: целочисленное значение переменной или defaultValue
func getEnvAsInt(key string, defaultValue int) int {
	valueStr := os.Getenv(key)
	if valueStr == "" {
		return defaultValue
	}
	var value int
	fmt.Sscanf(valueStr, "%d", &value)
	return value
}
