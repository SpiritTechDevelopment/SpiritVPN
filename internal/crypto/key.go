package crypto

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// KeySize — размер ключа AES-256.
const KeySize = 32

// maxKeyIDLen — верхняя граница key_id; он хранится в колонке
// vpn_accesses.encryption_key_id и попадает в AAD.
const maxKeyIDLen = 64

var (
	// ErrKeyMissing — ключ не задан. Production secrets не имеют default values,
	// поэтому отсутствие ключа — ошибка старта, а не повод сгенерировать
	// временный.
	ErrKeyMissing = errors.New("crypto: encryption key не задан")

	// ErrKeyMalformed — ключ задан, но не разбирается: неверный формат, битый
	// base64 или длина не 32 байта.
	ErrKeyMalformed = errors.New("crypto: encryption key некорректен")

	// ErrKeyIDInvalid — key_id пуст, длиннее 64 символов или содержит символы вне
	// [A-Za-z0-9._-].
	ErrKeyIDInvalid = errors.New("crypto: key_id некорректен")
)

// Key — active encryption key вместе со своей идентичностью.
//
// v1 держит ровно один active key; key_id сохраняется в каждой записи ради
// forward-compat, но ротация и decrypt-only ключи в v1 не реализуются.
type Key struct {
	id     string
	secret []byte
}

// NewKey собирает ключ из идентификатора и сырых 32 байт.
func NewKey(id string, secret []byte) (Key, error) {
	if err := validateKeyID(id); err != nil {
		return Key{}, err
	}
	if len(secret) != KeySize {
		return Key{}, fmt.Errorf("%w: длина %d, ожидалось %d байт", ErrKeyMalformed, len(secret), KeySize)
	}

	// Копия: вызывающий может переиспользовать или обнулить свой буфер.
	owned := make([]byte, KeySize)
	copy(owned, secret)

	return Key{id: id, secret: owned}, nil
}

// ParseKey разбирает ключ из строки `<key_id>:<base64 32 байта>`.
//
// Формат один на env и на файл, чтобы key_id всегда приезжал вместе с секретом:
// запись, зашифрованная неизвестно каким ключом, нерасшифровываема, а колонка
// encryption_key_id как раз и существует, чтобы этого не случилось.
func ParseKey(raw string) (Key, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Key{}, ErrKeyMissing
	}

	id, encoded, found := strings.Cut(raw, ":")
	if !found {
		return Key{}, fmt.Errorf("%w: ожидался формат <key_id>:<base64>", ErrKeyMalformed)
	}

	secret, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		// Сама ошибка декодера не прикладывается: она содержит фрагмент секрета.
		return Key{}, fmt.Errorf("%w: секрет не является корректным base64", ErrKeyMalformed)
	}

	return NewKey(id, secret)
}

// LoadKey читает active key из окружения: сначала `<name>_FILE` (путь к файлу с
// ограниченными правами), затем `<name>`.
//
// Значения по умолчанию отсутствуют, ключ не принимается аргументом командной
// строки и никогда не печатается при старте. Настоящий secret-провайдер
// придёт отдельно; этот путь предназначен для dev и для монтируемых секретов.
func LoadKey(getenv func(string) string, name string) (Key, error) {
	if path := strings.TrimSpace(getenv(name + "_FILE")); path != "" {
		content, err := os.ReadFile(path)
		if err != nil {
			return Key{}, fmt.Errorf("crypto: чтение %s_FILE: %w", name, err)
		}
		return ParseKey(string(content))
	}

	raw := getenv(name)
	if strings.TrimSpace(raw) == "" {
		return Key{}, fmt.Errorf("%w: задайте %s или %s_FILE", ErrKeyMissing, name, name)
	}

	return ParseKey(raw)
}

// ID — идентификатор ключа для колонки encryption_key_id.
func (k Key) ID() string {
	return k.id
}

// IsZero сообщает, что ключ не заполнен.
func (k Key) IsZero() bool {
	return len(k.secret) == 0
}

// String печатает только идентификатор: секрет не должен попадать в логи ни
// прямо, ни через печать вмещающей структуры.
func (k Key) String() string {
	if k.IsZero() {
		return "encryption_key(empty)"
	}
	return "encryption_key(" + k.id + ")"
}

// GoString реализует fmt.GoStringer; как и у ClientUUID, у fmt приоритет выше у
// Formatter, так что %#v фактически закрывает Format.
func (k Key) GoString() string {
	return k.String()
}

// Format перехватывает у fmt все глаголы разом.
//
// String закрывает только v, s, q, x и X — на остальных fmt печатает поля
// структуры сам, и %d, %b, %o, %U или %c выдали бы секрет побайтно. Проверено
// поведением fmt, а не выведено из документации.
func (k Key) Format(f fmt.State, verb rune) {
	if verb == 'q' {
		fmt.Fprintf(f, "%q", k.String())
		return
	}
	_, _ = io.WriteString(f, k.String())
}

func validateKeyID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: пустой", ErrKeyIDInvalid)
	}
	if len(id) > maxKeyIDLen {
		return fmt.Errorf("%w: длина %d, максимум %d", ErrKeyIDInvalid, len(id), maxKeyIDLen)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
		default:
			return fmt.Errorf("%w: недопустимый символ %q", ErrKeyIDInvalid, r)
		}
	}
	return nil
}
