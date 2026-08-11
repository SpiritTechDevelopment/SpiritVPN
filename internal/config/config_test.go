package config_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RomanRyabinkin/SpiritVPN/internal/config"
	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
)

// dsnPassword — пароль внутри тестового DSN; тесты следят, чтобы он не всплыл
// ни в одном виде печати конфигурации.
const dsnPassword = "sup3rsecret"

// keySecret — заведомо ненастоящий секрет ключа шифрования.
var keySecret = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xAB}, crypto.KeySize))

// testKey — значение SPIRIT_CLIENT_UUID_KEY в формате <key_id>:<base64>.
var testKey = "test-key:" + keySecret

// validEnv — минимальное окружение, при котором Load обязан пройти. Тесты
// меняют в нём только то, что проверяют.
func validEnv() map[string]string {
	return map[string]string{
		config.EnvDatabaseURL:   fmt.Sprintf("postgres://spirit:%s@db:5432/spirit", dsnPassword),
		config.EnvGRPCCertFile:  "/tls/server.crt",
		config.EnvGRPCKeyFile:   "/tls/server.key",
		config.EnvGRPCClientCA:  "/tls/ca.crt",
		config.EnvAgentCertFile: "/tls/agent-client.crt",
		config.EnvAgentKeyFile:  "/tls/agent-client.key",
		config.EnvAgentCAFile:   "/tls/agent-ca.crt",
		config.EnvRoleWriter:    "product-svc",
		config.EnvClientUUIDKey: testKey,
	}
}

func getenv(env map[string]string) func(string) string {
	return func(name string) string { return env[name] }
}

func mustLoad(t *testing.T, env map[string]string) config.Config {
	t.Helper()

	cfg, err := config.Load(getenv(env))
	if err != nil {
		t.Fatalf("неожиданная ошибка загрузки: %v", err)
	}
	return cfg
}

// TestLoadAppliesDefaults — умолчания есть только у не-секретов (§14).
func TestLoadAppliesDefaults(t *testing.T) {
	cfg := mustLoad(t, validEnv())

	if cfg.Log.Level != slog.LevelInfo {
		t.Errorf("уровень %v, ожидался info", cfg.Log.Level)
	}
	if cfg.GRPC.Listen != ":8443" {
		t.Errorf("grpc listen %q, ожидался :8443", cfg.GRPC.Listen)
	}
	if cfg.HTTP.Listen != ":8080" {
		t.Errorf("http listen %q, ожидался :8080", cfg.HTTP.Listen)
	}
	if cfg.Postgres.MaxConns != 10 {
		t.Errorf("max conns %d, ожидалось 10", cfg.Postgres.MaxConns)
	}
}

// TestLoadReadsAllValues — заданные значения доезжают без искажений.
func TestLoadReadsAllValues(t *testing.T) {
	env := validEnv()
	env[config.EnvLogLevel] = "debug"
	env[config.EnvDBMaxConns] = "25"
	env[config.EnvGRPCListen] = "127.0.0.1:9443"
	env[config.EnvHTTPListen] = "127.0.0.1:9090"
	env[config.EnvRoleReader] = "product-svc,support-portal"

	cfg := mustLoad(t, env)

	if cfg.Log.Level != slog.LevelDebug {
		t.Errorf("уровень %v, ожидался debug", cfg.Log.Level)
	}
	if cfg.Postgres.MaxConns != 25 {
		t.Errorf("max conns %d, ожидалось 25", cfg.Postgres.MaxConns)
	}
	if cfg.GRPC.Listen != "127.0.0.1:9443" || cfg.HTTP.Listen != "127.0.0.1:9090" {
		t.Errorf("адреса %q и %q", cfg.GRPC.Listen, cfg.HTTP.Listen)
	}
	if cfg.ClientUUIDKey.ID() != "test-key" {
		t.Errorf("key_id %q, ожидался test-key", cfg.ClientUUIDKey.ID())
	}

	wantReaders := []string{"product-svc", "support-portal"}
	if len(cfg.GRPC.CustomerAccessReaders) != len(wantReaders) {
		t.Fatalf("readers %v, ожидалось %v", cfg.GRPC.CustomerAccessReaders, wantReaders)
	}
	for i, want := range wantReaders {
		if cfg.GRPC.CustomerAccessReaders[i] != want {
			t.Errorf("reader[%d] = %q, ожидался %q", i, cfg.GRPC.CustomerAccessReaders[i], want)
		}
	}
}

// TestLoadReportsEveryMissingVariable — ошибки собираются все разом, чтобы
// оператор увидел полный список за один запуск, а не по одной за перезапуск.
func TestLoadReportsEveryMissingVariable(t *testing.T) {
	cfg, err := config.Load(getenv(map[string]string{}))
	if err == nil {
		t.Fatal("пустое окружение обязано провалить старт")
	}
	if !errors.Is(err, config.ErrMissing) {
		t.Errorf("ошибка не опознаётся как ErrMissing: %v", err)
	}
	// Config несравним (внутри слайсы), поэтому проверяются поля, которые при
	// частичном заполнении оказались бы непустыми.
	if cfg.Postgres.URL != "" || cfg.GRPC.Listen != "" || !cfg.ClientUUIDKey.IsZero() {
		t.Errorf("при ошибке наружу ушла частично заполненная конфигурация: %v", cfg)
	}

	for _, name := range []string{
		config.EnvDatabaseURL,
		config.EnvGRPCCertFile,
		config.EnvGRPCKeyFile,
		config.EnvGRPCClientCA,
		config.EnvAgentCertFile,
		config.EnvAgentKeyFile,
		config.EnvAgentCAFile,
		config.EnvClientUUIDKey,
	} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("в списке ошибок нет %s: %v", name, err)
		}
	}
}

// TestLoadRejectsEmptyRoleLists — сервер, отвечающий PERMISSION_DENIED на всё,
// не должен подняться молча.
func TestLoadRejectsEmptyRoleLists(t *testing.T) {
	env := validEnv()
	delete(env, config.EnvRoleWriter)

	if _, err := config.Load(getenv(env)); err == nil {
		t.Fatal("конфигурация без единой идентичности обязана провалить старт")
	}
}

// TestLoadDropsEmptyIdentities — решение 13: пустая идентичность в списке
// совпала бы с пустым DNS SAN и раздала бы роль любому валидному сертификату.
func TestLoadDropsEmptyIdentities(t *testing.T) {
	env := validEnv()
	env[config.EnvRoleWriter] = " product-svc , , ,"
	env[config.EnvRoleReader] = ",,"

	cfg := mustLoad(t, env)

	if len(cfg.GRPC.CustomerAccessWriters) != 1 || cfg.GRPC.CustomerAccessWriters[0] != "product-svc" {
		t.Errorf("writers %#v, ожидался ровно [product-svc]", cfg.GRPC.CustomerAccessWriters)
	}
	if len(cfg.GRPC.CustomerAccessReaders) != 0 {
		t.Errorf("readers %#v, ожидался пустой список", cfg.GRPC.CustomerAccessReaders)
	}
}

// TestLoadRejectsInvalidValues — таблица «мусор в переменной → отказ старта».
func TestLoadRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"неизвестный уровень", config.EnvLogLevel, "verbose"},
		{"нечисловой пул", config.EnvDBMaxConns, "много"},
		{"нулевой пул", config.EnvDBMaxConns, "0"},
		{"отрицательный пул", config.EnvDBMaxConns, "-1"},
		{"grpc без порта", config.EnvGRPCListen, "localhost"},
		{"http без порта", config.EnvHTTPListen, "8080"},
		{"ключ без key_id", config.EnvClientUUIDKey, keySecret},
		{"ключ неверной длины", config.EnvClientUUIDKey, "test-key:" + base64.StdEncoding.EncodeToString([]byte("коротко"))},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := validEnv()
			env[tc.key] = tc.value

			if _, err := config.Load(getenv(env)); err == nil {
				t.Fatalf("%s=%q обязано провалить старт", tc.key, tc.value)
			}
		})
	}
}

// TestLoadAcceptsKeyFromFile — _FILE приоритетнее прямого значения: §14
// предпочитает защищённый файл переменной окружения.
func TestLoadAcceptsKeyFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_uuid.key")
	if err := os.WriteFile(path, []byte("from-file:"+keySecret+"\n"), 0o600); err != nil {
		t.Fatalf("подготовка файла ключа: %v", err)
	}

	env := validEnv()
	env[config.EnvClientUUIDKey+"_FILE"] = path

	cfg := mustLoad(t, env)

	if cfg.ClientUUIDKey.ID() != "from-file" {
		t.Errorf("key_id %q, ожидался from-file — значение из _FILE обязано победить", cfg.ClientUUIDKey.ID())
	}
}

// TestConfigNeverPrintsSecrets — §14: конфигурация не выводится при старте.
// Проверяются оба реальных пути утечки: структурный лог и печать через fmt.
func TestConfigNeverPrintsSecrets(t *testing.T) {
	cfg := mustLoad(t, validEnv())

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("старт", slog.Any("config", cfg))

	rendered := map[string]string{
		"slog":  buf.String(),
		"%v":    fmt.Sprintf("%v", cfg),
		"%+v":   fmt.Sprintf("%+v", cfg),
		"%s":    fmt.Sprintf("%s", cfg),
		"%q":    fmt.Sprintf("%q", cfg),
		"Error": cfg.String(),
	}

	for where, text := range rendered {
		for _, secret := range []string{dsnPassword, keySecret} {
			if strings.Contains(text, secret) {
				t.Errorf("%s раскрывает секрет: %s", where, text)
			}
		}
	}

	// Полезное при этом остаётся видимым, иначе лог бесполезен.
	if !strings.Contains(buf.String(), "test-key") {
		t.Error("в логе нет encryption_key_id, по нему сверяют, каким ключом зашифрованы записи")
	}
	if !strings.Contains(buf.String(), ":8443") {
		t.Error("в логе нет адреса прослушивания")
	}
}
