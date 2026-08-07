package crypto

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testSecret — 32 байта 0x01..0x20; узнаваемые значения нужны, чтобы отличить
// утечку секрета в текстовом выводе.
var testSecret = func() []byte {
	secret := make([]byte, KeySize)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	return secret
}()

var testSecretB64 = base64.StdEncoding.EncodeToString(testSecret)

func envFunc(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestParseKey(t *testing.T) {
	key, err := ParseKey("dev-1:" + testSecretB64)
	if err != nil {
		t.Fatalf("ParseKey() ошибка: %v", err)
	}

	if key.ID() != "dev-1" {
		t.Fatalf("ID() = %q, ожидался dev-1", key.ID())
	}
	if key.IsZero() {
		t.Fatal("IsZero() = true у разобранного ключа")
	}
	if len(key.secret) != KeySize {
		t.Fatalf("длина секрета = %d, ожидалось %d", len(key.secret), KeySize)
	}
}

func TestParseKeyRejectsMalformed(t *testing.T) {
	shortB64 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x01}, KeySize-1))
	longB64 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x01}, KeySize+1))

	cases := []struct {
		name string
		raw  string
		want error
	}{
		{"пустая строка", "", ErrKeyMissing},
		{"только пробелы", "   \n", ErrKeyMissing},
		{"без разделителя", testSecretB64, ErrKeyMalformed},
		{"битый base64", "dev-1:не-base64!!", ErrKeyMalformed},
		{"короткий секрет", "dev-1:" + shortB64, ErrKeyMalformed},
		{"длинный секрет", "dev-1:" + longB64, ErrKeyMalformed},
		{"пустой секрет", "dev-1:", ErrKeyMalformed},
		{"пустой key_id", ":" + testSecretB64, ErrKeyIDInvalid},
		{"недопустимый символ в key_id", "dev 1:" + testSecretB64, ErrKeyIDInvalid},
		{"слишком длинный key_id", strings.Repeat("k", maxKeyIDLen+1) + ":" + testSecretB64, ErrKeyIDInvalid},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseKey(tc.raw)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ошибка = %v, ожидалась %v", err, tc.want)
			}
		})
	}
}

// Ошибка разбора не должна нести сам секрет: она попадает в лог старта.
func TestParseKeyErrorHasNoSecret(t *testing.T) {
	_, err := ParseKey("dev-1:" + testSecretB64 + "==")
	if err == nil {
		t.Fatal("ParseKey() принял битый base64")
	}
	if strings.Contains(err.Error(), testSecretB64[:8]) {
		t.Fatalf("ошибка %q содержит фрагмент секрета", err.Error())
	}
}

func TestNewKeyCopiesSecret(t *testing.T) {
	secret := bytes.Repeat([]byte{0x07}, KeySize)

	key, err := NewKey("dev-1", secret)
	if err != nil {
		t.Fatalf("NewKey() ошибка: %v", err)
	}

	// Вызывающий вправе обнулить свой буфер сразу после конструктора.
	for i := range secret {
		secret[i] = 0
	}

	if bytes.Equal(key.secret, secret) {
		t.Fatal("ключ разделяет буфер с вызывающим")
	}
}

// Секрет не должен появляться в логах ни прямо, ни через печать вмещающей
// структуры (§14).
func TestKeyDoesNotLeakSecret(t *testing.T) {
	key, err := ParseKey("dev-1:" + testSecretB64)
	if err != nil {
		t.Fatalf("ParseKey() ошибка: %v", err)
	}

	type holder struct {
		Name string
		Key  Key
	}

	// Без строковых полей: только на такой структуре числовой глагол проходит
	// go vet, а он и есть опасный путь печати.
	type numericHolder struct {
		Generation int
		Key        Key
	}

	outputs := []string{
		key.String(),
		key.GoString(),
		fmt.Sprintf("%v", key),
		fmt.Sprintf("%+v", key),
		fmt.Sprintf("%#v", key),
		fmt.Sprintf("%s", key),
		fmt.Sprintf("%q", key),
		fmt.Sprintf("%x", key),
		fmt.Sprintf("%X", key),
		// Числовые глаголы — самые опасные: String их не закрывает, и без Format
		// fmt напечатал бы неэкспортированные поля структуры побайтно.
		fmt.Sprintf("%d", key),
		fmt.Sprintf("%b", key),
		fmt.Sprintf("%o", key),
		fmt.Sprintf("%U", key),
		fmt.Sprintf("%c", key),
		fmt.Sprintf("%v", holder{Name: "active", Key: key}),
		fmt.Sprintf("%+v", holder{Name: "active", Key: key}),
		fmt.Sprintf("%#v", holder{Name: "active", Key: key}),
		fmt.Sprintf("%x", holder{Name: "active", Key: key}),
		fmt.Sprintf("%d", numericHolder{Generation: 1, Key: key}),
	}

	// hexSecret — то, во что превратился бы секрет под %x.
	hexSecret := fmt.Sprintf("%x", testSecret[:8])

	for i, got := range outputs {
		if strings.Contains(got, testSecretB64[:8]) {
			t.Fatalf("вывод %d = %q содержит секрет", i, got)
		}
		if strings.Contains(strings.ToLower(got), hexSecret) {
			t.Fatalf("вывод %d = %q содержит секрет в hex", i, got)
		}
		// Байты секрета в десятичном виде тоже утечка.
		if strings.Contains(got, "1, 2, 3, 4") || strings.Contains(got, "[1 2 3 4") {
			t.Fatalf("вывод %d = %q содержит байты секрета", i, got)
		}
		if !strings.Contains(got, "encryption_key") {
			t.Fatalf("вывод %d = %q, ожидалась редакция", i, got)
		}
	}

	if got := (Key{}).String(); !strings.Contains(got, "empty") {
		t.Fatalf("String() пустого ключа = %q", got)
	}
}

func TestLoadKeyFromEnv(t *testing.T) {
	getenv := envFunc(map[string]string{"SPIRIT_KEY": "dev-1:" + testSecretB64})

	key, err := LoadKey(getenv, "SPIRIT_KEY")
	if err != nil {
		t.Fatalf("LoadKey() ошибка: %v", err)
	}
	if key.ID() != "dev-1" {
		t.Fatalf("ID() = %q, ожидался dev-1", key.ID())
	}
}

// Файл имеет приоритет над переменной: смонтированный секрет должен побеждать
// значение, случайно оставшееся в окружении.
func TestLoadKeyFromFileWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("from-file:"+testSecretB64+"\n"), 0o600); err != nil {
		t.Fatalf("подготовка файла: %v", err)
	}

	getenv := envFunc(map[string]string{
		"SPIRIT_KEY":      "from-env:" + testSecretB64,
		"SPIRIT_KEY_FILE": path,
	})

	key, err := LoadKey(getenv, "SPIRIT_KEY")
	if err != nil {
		t.Fatalf("LoadKey() ошибка: %v", err)
	}
	if key.ID() != "from-file" {
		t.Fatalf("ID() = %q, ожидался from-file", key.ID())
	}
}

// Default value у ключа нет (§14): пустое окружение обязано провалить старт.
func TestLoadKeyRequiresValue(t *testing.T) {
	if _, err := LoadKey(envFunc(nil), "SPIRIT_KEY"); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("ошибка = %v, ожидалась ErrKeyMissing", err)
	}

	getenv := envFunc(map[string]string{"SPIRIT_KEY": "   "})
	if _, err := LoadKey(getenv, "SPIRIT_KEY"); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("ошибка = %v, ожидалась ErrKeyMissing", err)
	}
}

func TestLoadKeyMissingFile(t *testing.T) {
	getenv := envFunc(map[string]string{
		"SPIRIT_KEY_FILE": filepath.Join(t.TempDir(), "нет-такого"),
	})

	if _, err := LoadKey(getenv, "SPIRIT_KEY"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ошибка = %v, ожидался os.ErrNotExist", err)
	}
}
