package config

import (
	"os"
	"testing"
	"time"
)

const (
	testDBHost     = "localhost"
	testDBPort     = "5432"
	testDBUser     = "testuser"
	testDBPassword = "testpass"
	testDBName     = "testdb"
	testJWTSecret  = "test_secret_key"
	testCustomHost = "custom_host"
	testCustomPort = 3306
	testAPIAddress = ":9090"
	testVPNHost    = "vpn.example.com"
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
				"DB_HOST":     testDBHost,
				"DB_PORT":     testDBPort,
				"DB_USER":     testDBUser,
				"DB_PASSWORD": testDBPassword,
				"DB_NAME":     testDBName,
				"JWT_SECRET":  testJWTSecret,
			},
			wantErr: false,
		},
		{
			name:    "valid with default values",
			envVars: map[string]string{},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Clearenv()

			for key, value := range tt.envVars {
				_ = os.Setenv(key, value)
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
		{"VPN Inbound Tag", cfg.VPN.InboundTag, "vless-in"},
		{"VPN Node Name", cfg.VPN.NodeName, "entry-1"},
		{"VPN Endpoints File", cfg.VPN.EndpointsFile, ""},
		{"VPN Fingerprint", cfg.VPN.Fingerprint, "chrome"},
		{"VPN Test Access TTL", cfg.VPN.TestAccessTTL, 24 * time.Hour},
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
	_ = os.Setenv("DB_HOST", testCustomHost)
	_ = os.Setenv("DB_PORT", "3306")
	_ = os.Setenv("API_ADDRESS", testAPIAddress)
	_ = os.Setenv("VPN_HOST", testVPNHost)

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
				_ = os.Setenv(tt.key, tt.value)
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
				_ = os.Setenv(tt.key, tt.value)
			}

			result := getEnvAsInt(tt.key, tt.defaultValue)
			if result != tt.expected {
				t.Errorf("getEnvAsInt() = %v, want %v", result, tt.expected)
			}
		})
	}
}

// TestGetEnvAsDuration проверяет корректность парсинга duration из переменных окружения.
// Тестирует валидные форматы, невалидные значения и значения по умолчанию.
func TestGetEnvAsDuration(t *testing.T) {
	tests := []struct {
		name         string
		key          string
		value        string
		defaultValue time.Duration
		expected     time.Duration
	}{
		{
			name:         "valid duration - minutes",
			key:          "TEST_DURATION",
			value:        "5m",
			defaultValue: 1 * time.Minute,
			expected:     5 * time.Minute,
		},
		{
			name:         "valid duration - seconds",
			key:          "TEST_DURATION",
			value:        "30s",
			defaultValue: 1 * time.Minute,
			expected:     30 * time.Second,
		},
		{
			name:         "valid duration - hours",
			key:          "TEST_DURATION",
			value:        "2h",
			defaultValue: 1 * time.Minute,
			expected:     2 * time.Hour,
		},
		{
			name:         "valid duration - complex",
			key:          "TEST_DURATION",
			value:        "1h30m",
			defaultValue: 1 * time.Minute,
			expected:     90 * time.Minute,
		},
		{
			name:         "invalid duration",
			key:          "TEST_DURATION",
			value:        "invalid",
			defaultValue: 5 * time.Minute,
			expected:     5 * time.Minute,
		},
		{
			name:         "missing variable",
			key:          "MISSING_DURATION",
			value:        "",
			defaultValue: 10 * time.Minute,
			expected:     10 * time.Minute,
		},
		{
			name:         "empty value",
			key:          "TEST_DURATION",
			value:        "",
			defaultValue: 3 * time.Minute,
			expected:     3 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Clearenv()
			if tt.value != "" {
				_ = os.Setenv(tt.key, tt.value)
			}

			result := getEnvAsDuration(tt.key, tt.defaultValue)
			if result != tt.expected {
				t.Errorf("getEnvAsDuration() = %v, want %v", result, tt.expected)
			}
		})
	}
}

// TestVPNConfig_StatsInterval проверяет что VPN_STATS_INTERVAL корректно парсится.
func TestVPNConfig_StatsInterval(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		expected time.Duration
	}{
		{
			name:     "default value when not set",
			envValue: "",
			expected: 5 * time.Minute,
		},
		{
			name:     "custom value 10 minutes",
			envValue: "10m",
			expected: 10 * time.Minute,
		},
		{
			name:     "custom value 1 hour",
			envValue: "1h",
			expected: 1 * time.Hour,
		},
		{
			name:     "custom value 30 seconds",
			envValue: "30s",
			expected: 30 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Clearenv()
			if tt.envValue != "" {
				_ = os.Setenv("VPN_STATS_INTERVAL", tt.envValue)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() failed: %v", err)
			}

			if cfg.VPN.StatsInterval != tt.expected {
				t.Errorf("VPN.StatsInterval = %v, want %v", cfg.VPN.StatsInterval, tt.expected)
			}
		})
	}
}
