// Package config собирает конфигурацию процесса из окружения.
//
// Правила, которым подчинён весь пакет:
//
//   - секреты не имеют значений по умолчанию. Отсутствие обязательной переменной
//     обязано провалить старт, а не подставить «разумный» дефолт: тихо
//     подставленный дефолт секрета — это работающий процесс с неверными
//     учётными данными;
//   - конфигурация читается только из окружения; аргументами командной строки
//     секреты не передаются, потому что они видны в ps и попадают в историю
//     оболочки;
//   - конфигурация не печатается целиком. String и LogValue редактируют DSN,
//     ключ шифрования редактирует себя сам (crypto.Key).
//
// Значения по умолчанию есть только у не-секретов: уровень логирования и адреса
// прослушивания.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"

	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
)

// Имена переменных окружения. Один префикс SPIRIT_ на весь процесс, чтобы
// конфигурация backend не смешивалась с чужими переменными в общем окружении
// контейнера.
const (
	EnvLogLevel      = "SPIRIT_LOG_LEVEL"
	EnvDatabaseURL   = "SPIRIT_DATABASE_URL"
	EnvDBMaxConns    = "SPIRIT_DB_MAX_CONNS"
	EnvGRPCListen    = "SPIRIT_GRPC_LISTEN"
	EnvGRPCCertFile  = "SPIRIT_GRPC_TLS_CERT_FILE"
	EnvGRPCKeyFile   = "SPIRIT_GRPC_TLS_KEY_FILE"
	EnvGRPCClientCA  = "SPIRIT_GRPC_TLS_CLIENT_CA_FILE"
	EnvRoleWriter    = "SPIRIT_ROLE_CUSTOMER_ACCESS_WRITER"
	EnvRoleReader    = "SPIRIT_ROLE_CUSTOMER_ACCESS_READER"
	EnvRoleManifest  = "SPIRIT_ROLE_MANIFEST_WRITER"
	EnvHTTPListen    = "SPIRIT_HTTP_LISTEN"
	EnvClientUUIDKey = "SPIRIT_CLIENT_UUID_KEY"

	// Клиентская пара для походов к агентам — отдельная от серверной.
	EnvAgentCertFile = "SPIRIT_AGENT_TLS_CERT_FILE"
	EnvAgentKeyFile  = "SPIRIT_AGENT_TLS_KEY_FILE"
	EnvAgentCAFile   = "SPIRIT_AGENT_TLS_CA_FILE"
)

// Значения по умолчанию для не-секретов.
const (
	defaultLogLevel   = "info"
	defaultGRPCListen = ":8443"
	defaultHTTPListen = ":8080"
	defaultDBMaxConns = 10
)

// redacted подставляется вместо секрета при печати.
const redacted = "[REDACTED]"

var (
	// ErrMissing — обязательная переменная не задана.
	ErrMissing = errors.New("config: обязательная переменная не задана")

	// ErrInvalid — переменная задана, но значение не разбирается.
	ErrInvalid = errors.New("config: некорректное значение")
)

// Config — полная конфигурация процесса.
type Config struct {
	Log      Log
	Postgres Postgres
	GRPC     GRPC
	HTTP     HTTP
	Agent    Agent

	// ClientUUIDKey — единственный active encryption key. Ротация и
	// decrypt-only ключи в v1 не поддерживаются.
	ClientUUIDKey crypto.Key
}

// Log — параметры логирования.
type Log struct {
	Level slog.Level
}

// Postgres — подключение к базе.
type Postgres struct {
	// URL содержит пароль, то есть является секретом целиком. Хранится строкой,
	// потому что именно строку принимает pgxpool; редактируется в String и
	// LogValue вмещающего Config.
	URL string

	// MaxConns — верхняя граница пула. Транзакции короткие, поэтому пул
	// упирается не в длительность работы, а в число одновременных команд.
	MaxConns int32
}

// GRPC — внешняя командно-запросная поверхность и её mTLS.
type GRPC struct {
	Listen string

	// CertFile/KeyFile — серверная пара, ClientCAFile — CA, которым подписаны
	// сертификаты клиентов. Приватный ключ читается процессом с диска и в
	// конфигурации не хранится.
	CertFile     string
	KeyFile      string
	ClientCAFile string

	// Идентичности по ролям. Роли независимы: writer не подразумевает
	// reader, потому что чтение отдаёт VLESS URI с client_uuid, то есть сами
	// credentials. Сервис, которому нужно и то и другое, перечисляется
	// в обоих списках явно.
	CustomerAccessWriters []string
	CustomerAccessReaders []string

	// ManifestWriters — identity infrastructure CI/CD. Роль отдельная от
	// customer-ролей: манифест переписывает топологию целиком, и продуктовому
	// сервису она не нужна ни в каком виде.
	ManifestWriters []string
}

// HTTP — служебная поверхность: health, readiness и метрики. Отдельный
// порт от gRPC намеренно: liveness обязан отвечать, даже когда gRPC не принимает
// соединения, и наружу этот порт не публикуется.
type HTTP struct {
	Listen string
}

// Agent — mTLS до node-agent'ов поверх management WireGuard overlay.
//
// Пара отдельная от GRPC намеренно: там backend принимает
// product-сервис и предъявляет себя ему, здесь — сам приходит клиентом к агенту.
// Общий сертификат смешал бы две роли, и компрометация одной автоматически
// означала бы компрометацию другой. CAFile — CA инфраструктуры, которым подписаны
// сертификаты агентов; серверному ClientCAFile он не равен.
type Agent struct {
	CertFile string
	KeyFile  string
	CAFile   string
}

// Load собирает конфигурацию, читая окружение через getenv.
//
// getenv параметром, а не os.Getenv напрямую: тесты обязаны проверять разбор без
// правки окружения процесса, иначе они не могут идти параллельно. Тот же приём
// уже применён в crypto.LoadKey.
//
// Ошибки собираются все разом через errors.Join, а не возвращаются на первой.
// При старте это разница между «поправил переменную, перезапустил, узнал про
// следующую» и одним полным списком того, чего не хватает.
func Load(getenv func(string) string) (Config, error) {
	var (
		cfg  Config
		errs []error
	)

	level, err := logLevel(value(getenv, EnvLogLevel, defaultLogLevel))
	if err != nil {
		errs = append(errs, err)
	}
	cfg.Log.Level = level

	cfg.Postgres.URL = required(getenv, EnvDatabaseURL, &errs)

	maxConns, err := dbMaxConns(getenv(EnvDBMaxConns))
	if err != nil {
		errs = append(errs, err)
	}
	cfg.Postgres.MaxConns = maxConns

	cfg.GRPC.Listen = listenAddr(value(getenv, EnvGRPCListen, defaultGRPCListen), EnvGRPCListen, &errs)
	cfg.GRPC.CertFile = required(getenv, EnvGRPCCertFile, &errs)
	cfg.GRPC.KeyFile = required(getenv, EnvGRPCKeyFile, &errs)
	cfg.GRPC.ClientCAFile = required(getenv, EnvGRPCClientCA, &errs)
	cfg.GRPC.CustomerAccessWriters = identities(getenv(EnvRoleWriter))
	cfg.GRPC.CustomerAccessReaders = identities(getenv(EnvRoleReader))
	cfg.GRPC.ManifestWriters = identities(getenv(EnvRoleManifest))

	// Конфигурация без единой идентичности поднимает сервер, который отвечает
	// PERMISSION_DENIED на каждый вызов. Формально это безопасно, но заметить
	// такое можно только по жалобам вызывающего, поэтому падаем на старте.
	if len(cfg.GRPC.CustomerAccessWriters) == 0 &&
		len(cfg.GRPC.CustomerAccessReaders) == 0 &&
		len(cfg.GRPC.ManifestWriters) == 0 {
		errs = append(errs, fmt.Errorf("%w: %s, %s и %s пусты, ни один клиент не сможет вызвать ни один метод",
			ErrMissing, EnvRoleWriter, EnvRoleReader, EnvRoleManifest))
	}

	cfg.Agent.CertFile = required(getenv, EnvAgentCertFile, &errs)
	cfg.Agent.KeyFile = required(getenv, EnvAgentKeyFile, &errs)
	cfg.Agent.CAFile = required(getenv, EnvAgentCAFile, &errs)

	cfg.HTTP.Listen = listenAddr(value(getenv, EnvHTTPListen, defaultHTTPListen), EnvHTTPListen, &errs)

	key, err := crypto.LoadKey(getenv, EnvClientUUIDKey)
	if err != nil {
		errs = append(errs, err)
	}
	cfg.ClientUUIDKey = key

	if err := errors.Join(errs...); err != nil {
		// Частично заполненная конфигурация наружу не отдаётся: вызывающий не
		// должен иметь возможности проигнорировать ошибку и подключиться
		// половиной значений.
		return Config{}, err
	}

	return cfg, nil
}

// value возвращает значение переменной либо fallback, если она пуста.
func value(getenv func(string) string, name, fallback string) string {
	if v := strings.TrimSpace(getenv(name)); v != "" {
		return v
	}
	return fallback
}

// required читает обязательную переменную, накапливая ошибку вместо возврата.
func required(getenv func(string) string, name string, errs *[]error) string {
	v := strings.TrimSpace(getenv(name))
	if v == "" {
		*errs = append(*errs, fmt.Errorf("%w: %s", ErrMissing, name))
	}
	return v
}

// logLevel разбирает уровень логирования. Регистр приводится к верхнему заранее:
// slog.Level.UnmarshalText ожидает DEBUG/INFO/WARN/ERROR, а в окружении уровень
// традиционно пишут строчными.
func logLevel(raw string) (slog.Level, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.ToUpper(raw))); err != nil {
		return 0, fmt.Errorf("%w: %s=%q, ожидалось debug|info|warn|error", ErrInvalid, EnvLogLevel, raw)
	}
	return level, nil
}

// dbMaxConns разбирает размер пула; пустое значение означает умолчание.
func dbMaxConns(raw string) (int32, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultDBMaxConns, nil
	}

	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%w: %s=%q, ожидалось целое > 0", ErrInvalid, EnvDBMaxConns, raw)
	}
	return int32(n), nil
}

// listenAddr проверяет адрес прослушивания на разбираемость.
//
// Проверка здесь, а не при вызове net.Listen, потому что опечатка в адресе —
// самая дешёвая ошибка конфигурации, и узнавать о ней надо до того, как процесс
// открыл соединение с базой и прочитал ключ шифрования.
func listenAddr(raw, name string, errs *[]error) string {
	if _, _, err := net.SplitHostPort(raw); err != nil {
		*errs = append(*errs, fmt.Errorf("%w: %s=%q, ожидалось host:port", ErrInvalid, name, raw))
		return ""
	}
	return raw
}

// identities разбирает список идентичностей клиентов, разделённый запятыми.
//
// Пустые элементы отбрасываются, и это не косметика. Лишняя запятая в
// SPIRIT_ROLE_CUSTOMER_ACCESS_WRITER=product-svc, даёт элемент "", а пустую
// идентичность вернёт сертификат без DNS SAN. Пустое совпало бы с пустым, и роль
// получил бы любой клиент с валидным сертификатом.
func identities(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// LogValue редактирует секреты при структурном логировании.
//
// Пишется группой, чтобы конфигурацию можно было залогировать одним атрибутом на
// старте и увидеть всё, кроме секретов: DSN заменяется целиком (в нём пароль), у
// ключа выводится только идентификатор.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("log_level", c.Log.Level.String()),
		slog.String("database_url", redacted),
		slog.Int("db_max_conns", int(c.Postgres.MaxConns)),
		slog.String("grpc_listen", c.GRPC.Listen),
		slog.String("grpc_cert_file", c.GRPC.CertFile),
		slog.String("grpc_key_file", c.GRPC.KeyFile),
		slog.String("grpc_client_ca_file", c.GRPC.ClientCAFile),
		slog.Int("customer_access_writers", len(c.GRPC.CustomerAccessWriters)),
		slog.Int("customer_access_readers", len(c.GRPC.CustomerAccessReaders)),
		slog.Int("manifest_writers", len(c.GRPC.ManifestWriters)),
		slog.String("http_listen", c.HTTP.Listen),
		slog.String("encryption_key_id", c.ClientUUIDKey.ID()),
	)
}

// String закрывает печать через fmt.
//
// Formatter, в отличие от crypto.Key, здесь не нужен: Stringer покрывает %v и
// %+v, которыми структуру и печатают, а числовые глаголы к структуре целиком не
// применяют. Секрет тут ровно один — DSN, и он не байтовый массив, который fmt
// мог бы вывести поэлементно в обход String.
func (c Config) String() string {
	return fmt.Sprintf(
		"config(log_level=%s database_url=%s db_max_conns=%d grpc_listen=%s http_listen=%s key=%s)",
		c.Log.Level, redacted, c.Postgres.MaxConns, c.GRPC.Listen, c.HTTP.Listen, c.ClientUUIDKey,
	)
}
