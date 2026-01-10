package config

import (
	"os"
	"testing"
)

const (
	testTelegramToken = "test_token_12345"
	testDBHost        = "localhost"
	testDBPort        = "5432"
	testDBUser        = "testuser"
	testDBPassword    = "testpass"
	testDBName        = "testdb"
	testJWTSecret     = "test_secret_key"
	testCustomHost    = "custom_host"
	testCustomPort    = 3306
	testAPIAddress    = ":9090"
	testVPNHost       = "vpn.example.com"
)

// TestLoad проверяет загрузку конфигурации из переменных окружения.
// Тестирует обязательные и опциональные параметры, а также значения по умолчанию.
func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		envVars map[string]string
		wantErr bool
	}{
		{
			name: "valid configuration",
			envVars: map[string]string{
				"DB_HOST":            testDBHost,
				"DB_PORT":            testDBPort,
				"DB_USER":            testDBUser,
				"DB_PASSWORD":        testDBPassword,
				"DB_NAME":            testDBName,
				"TELEGRAM_BOT_TOKEN": testTelegramToken,
				"JWT_SECRET":         testJWTSecret,
			},
			wantErr: false,
		},
		{
			name: "missing required telegram token",
			envVars: map[string]string{
				"DB_HOST": testDBHost,
			},
			wantErr: true,
		},
		{
			name: "valid with default values",
			envVars: map[string]string{
				"TELEGRAM_BOT_TOKEN": testTelegramToken,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Clearenv()

			for key, value := range tt.envVars {
				os.Setenv(key, value)
			}

			cfg, err := Load()

			if (err != nil) != tt.wantErr {
				t.Errorf("Load() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && cfg == nil {
				t.Error("Load() returned nil config")
			}

			if !tt.wantErr && cfg != nil {
				if cfg.Database.Port == 0 {
					t.Error("Database port should have default value")
				}
				if cfg.Redis.Port == 0 {
					t.Error("Redis port should have default value")
				}
			}
		})
	}
}

// TestLoadDefaults проверяет, что значения по умолчанию применяются корректно
// при отсутствии переменных окружения.
func TestLoadDefaults(t *testing.T) {
	os.Clearenv()
	os.Setenv("TELEGRAM_BOT_TOKEN", testTelegramToken)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	tests := []struct {
		name     string
		got      interface{}
		expected interface{}
	}{
		{"API Address", cfg.API.Address, ":8080"},
		{"API Mode", cfg.API.Mode, "debug"},
		{"Database Host", cfg.Database.Host, "localhost"},
		{"Database Port", cfg.Database.Port, 5432},
		{"Redis Host", cfg.Redis.Host, "localhost"},
		{"Redis Port", cfg.Redis.Port, 6379},
		{"Redis DB", cfg.Redis.DB, 0},
		{"VPN Port", cfg.VPN.Port, 443},
		{"VPN API Port", cfg.VPN.ApiPort, 10085},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.expected {
				t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.expected)
			}
		})
	}
}

// TestLoadCustomValues проверяет загрузку пользовательских значений,
// переопределяющих значения по умолчанию.
func TestLoadCustomValues(t *testing.T) {
	os.Clearenv()
	os.Setenv("TELEGRAM_BOT_TOKEN", testTelegramToken)
	os.Setenv("DB_HOST", testCustomHost)
	os.Setenv("DB_PORT", "3306")
	os.Setenv("API_ADDRESS", testAPIAddress)
	os.Setenv("VPN_HOST", testVPNHost)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.Database.Host != testCustomHost {
		t.Errorf("Database.Host = %v, want %v", cfg.Database.Host, testCustomHost)
	}
	if cfg.Database.Port != testCustomPort {
		t.Errorf("Database.Port = %v, want %v", cfg.Database.Port, testCustomPort)
	}
	if cfg.API.Address != testAPIAddress {
		t.Errorf("API.Address = %v, want %v", cfg.API.Address, testAPIAddress)
	}
	if cfg.VPN.Host != testVPNHost {
		t.Errorf("VPN.Host = %v, want %v", cfg.VPN.Host, testVPNHost)
	}
}

// TestGetEnv проверяет функцию получения переменной окружения
// с возможностью указания значения по умолчанию.
func TestGetEnv(t *testing.T) {
	tests := []struct {
		name         string
		key          string
		value        string
		defaultValue string
		expected     string
	}{
		{
			name:         "existing variable",
			key:          "TEST_VAR",
			value:        "test_value",
			defaultValue: "default",
			expected:     "test_value",
		},
		{
			name:         "missing variable",
			key:          "MISSING_VAR",
			value:        "",
			defaultValue: "default",
			expected:     "default",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Clearenv()
			if tt.value != "" {
				os.Setenv(tt.key, tt.value)
			}

			result := getEnv(tt.key, tt.defaultValue)
			if result != tt.expected {
				t.Errorf("getEnv() = %v, want %v", result, tt.expected)
			}
		})
	}
}

// TestGetEnvAsInt проверяет функцию получения переменной окружения как целого числа.
// Тестирует корректную обработку невалидных значений и возврат значения по умолчанию.
func TestGetEnvAsInt(t *testing.T) {
	tests := []struct {
		name         string
		key          string
		value        string
		defaultValue int
		expected     int
	}{
		{
			name:         "valid integer",
			key:          "TEST_INT",
			value:        "42",
			defaultValue: 10,
			expected:     42,
		},
		{
			name:         "invalid integer",
			key:          "TEST_INT",
			value:        "not_a_number",
			defaultValue: 10,
			expected:     10,
		},
		{
			name:         "missing variable",
			key:          "MISSING_INT",
			value:        "",
			defaultValue: 10,
			expected:     10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Clearenv()
			if tt.value != "" {
				os.Setenv(tt.key, tt.value)
			}

			result := getEnvAsInt(tt.key, tt.defaultValue)
			if result != tt.expected {
				t.Errorf("getEnvAsInt() = %v, want %v", result, tt.expected)
			}
		})
	}
}
