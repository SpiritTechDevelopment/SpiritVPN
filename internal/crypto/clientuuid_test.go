package crypto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// secretUUID — заведомо узнаваемое значение: его текстовая форма не должна
// встретиться ни в одном выводе.
var secretUUID = uuid.MustParse("3f2504e0-4f89-41d3-9a0c-0305e82c3301")

// leakCheck проверяет, что вывод не содержит ни канонической формы uuid, ни его
// hex-представления без дефисов, ни отдельного значащего фрагмента.
func leakCheck(t *testing.T, what, got string) {
	t.Helper()

	canonical := secretUUID.String()
	forbidden := []string{
		canonical,
		strings.ReplaceAll(canonical, "-", ""),
		"3f2504e0",
		"e82c3301",
		// Те же байты в десятичном виде — так их напечатали бы числовые глаголы.
		strings.Trim(fmt.Sprintf("%d", secretUUID[:4]), "[]"),
	}

	for _, needle := range forbidden {
		if strings.Contains(strings.ToLower(got), needle) {
			t.Fatalf("%s = %q содержит client_uuid (%q)", what, got, needle)
		}
	}
	if !strings.Contains(got, redactedClientUUID) {
		t.Fatalf("%s = %q, ожидалась редакция %q", what, got, redactedClientUUID)
	}
}

// wrapper — экспортированное поле: так ClientUUID и попадает в структуры,
// которые печатают целиком.
type wrapper struct {
	AccessID string
	Secret   ClientUUID
}

// numericWrapper — та же роль, но без строковых полей: только на такой структуре
// числовой глагол проходит go vet, и именно её печать проверяет, что Format
// вызывается и для вложенного значения.
type numericWrapper struct {
	Generation int
	Secret     ClientUUID
}

func TestClientUUIDDoesNotLeakInTextForms(t *testing.T) {
	secret := NewClientUUID(secretUUID)

	cases := []struct {
		name string
		got  string
	}{
		{"String()", secret.String()},
		{"GoString()", secret.GoString()},
		{"%v", fmt.Sprintf("%v", secret)},
		{"%s", fmt.Sprintf("%s", secret)},
		{"%q", fmt.Sprintf("%q", secret)},
		{"%#v", fmt.Sprintf("%#v", secret)},
		{"%x", fmt.Sprintf("%x", secret)},
		{"%X", fmt.Sprintf("%X", secret)},
		{"%+v", fmt.Sprintf("%+v", secret)},
		// Числовые глаголы String не закрывает: без Format здесь были бы байты
		// uuid в десятичном, двоичном и восьмеричном виде.
		{"%d", fmt.Sprintf("%d", secret)},
		{"%b", fmt.Sprintf("%b", secret)},
		{"%o", fmt.Sprintf("%o", secret)},
		{"%U", fmt.Sprintf("%U", secret)},
		{"%c", fmt.Sprintf("%c", secret)},
		{"структура %d", fmt.Sprintf("%d", numericWrapper{Generation: 1, Secret: secret})},
		{"fmt.Sprint", fmt.Sprint(secret)},
		{"структура %v", fmt.Sprintf("%v", wrapper{AccessID: "a", Secret: secret})},
		{"структура %+v", fmt.Sprintf("%+v", wrapper{AccessID: "a", Secret: secret})},
		{"структура %#v", fmt.Sprintf("%#v", wrapper{AccessID: "a", Secret: secret})},
		{"указатель на структуру", fmt.Sprintf("%v", &wrapper{AccessID: "a", Secret: secret})},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			leakCheck(t, tc.name, tc.got)
		})
	}
}

func TestClientUUIDDoesNotLeakInJSON(t *testing.T) {
	secret := NewClientUUID(secretUUID)

	direct, err := json.Marshal(secret)
	if err != nil {
		t.Fatalf("json.Marshal() ошибка: %v", err)
	}
	leakCheck(t, "json значения", string(direct))

	nested, err := json.Marshal(wrapper{AccessID: "a", Secret: secret})
	if err != nil {
		t.Fatalf("json.Marshal() ошибка: %v", err)
	}
	leakCheck(t, "json структуры", string(nested))

	text, err := secret.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText() ошибка: %v", err)
	}
	leakCheck(t, "MarshalText", string(text))
}

func TestClientUUIDDoesNotLeakInSlog(t *testing.T) {
	secret := NewClientUUID(secretUUID)

	for _, tc := range []struct {
		name    string
		handler func(*bytes.Buffer) slog.Handler
	}{
		{"text", func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) }},
		{"json", func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			slog.New(tc.handler(&buf)).Info("выдан access", "client_uuid", secret)
			leakCheck(t, "slog "+tc.name, buf.String())
		})
	}
}

// Reveal — единственный путь наружу, и он обязан отдавать исходное значение:
// иначе VLESS URI окажется нерабочей.
func TestClientUUIDReveal(t *testing.T) {
	secret := NewClientUUID(secretUUID)

	if got := secret.Reveal(); got != secretUUID {
		t.Fatalf("Reveal() = %v, ожидалось %v", got, secretUUID)
	}
	if secret.IsZero() {
		t.Fatal("IsZero() = true у заполненного значения")
	}
	if !(ClientUUID{}).IsZero() {
		t.Fatal("IsZero() = false у пустого значения")
	}
}
