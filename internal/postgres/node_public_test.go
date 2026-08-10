package postgres

import (
	"testing"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
)

// TestNodePublicFromPinsColumnFormat пиннит раскладку public_config (решение 19).
//
// Формат колонки фиксируется здесь, у единственного её читателя: срез приёма
// манифеста обязан будет писать ровно эти ключи. Тест буквальный намеренно —
// переименование ключа обязано провалить сборку, а не тихо превратить рабочую
// ноду в непригодную.
func TestNodePublicFromPinsColumnFormat(t *testing.T) {
	const raw = `{
	  "address": "nl.example.com",
	  "port": 443,
	  "reality_public_key": "pub-key",
	  "server_name": "www.example.org",
	  "short_id": "ab12",
	  "fingerprint": "chrome",
	  "transport": "tcp",
	  "flow": "xtls-rprx-vision",
	  "display_name": "Netherlands"
	}`

	want := app.NodePublic{
		Address:          "nl.example.com",
		Port:             443,
		RealityPublicKey: "pub-key",
		ServerName:       "www.example.org",
		ShortID:          "ab12",
		Fingerprint:      "chrome",
		Transport:        "tcp",
		Flow:             "xtls-rprx-vision",
		DisplayName:      "Netherlands",
	}

	if got := nodePublicFrom([]byte(raw)); got != want {
		t.Fatalf("разбор public_config\n  получено %+v\n  ожидалось %+v", got, want)
	}
}

// TestNodePublicFromDegradesOnBadInput — решение 18: нераспознанный jsonb не
// является ошибкой запроса. Нулевая структура не проходит Usable, и ссылка на
// этой ноде уходит наружу как FAILED, не задевая остальные.
func TestNodePublicFromDegradesOnBadInput(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"пустая колонка", ""},
		{"не объект", `"строка"`},
		{"битый json", `{"address":`},
		{"порт строкой", `{"address": "nl.example.com", "port": "443"}`},
		{"пустой объект", `{}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nodePublicFrom([]byte(tt.raw))
			if got.Usable() {
				t.Fatalf("%+v признана пригодной", got)
			}
		})
	}
}

// TestNodePublicFromIgnoresUnknownKeys — колонка переживает добавление полей
// манифестом более новой версии: лишний ключ не должен ломать чтение уже
// работающей ноды.
func TestNodePublicFromIgnoresUnknownKeys(t *testing.T) {
	const raw = `{
	  "address": "nl.example.com",
	  "port": 443,
	  "reality_public_key": "pub-key",
	  "server_name": "www.example.org",
	  "fingerprint": "chrome",
	  "transport": "tcp",
	  "flow": "xtls-rprx-vision",
	  "display_name": "Netherlands",
	  "поле_из_будущего": {"вложенное": 1}
	}`

	got := nodePublicFrom([]byte(raw))
	if !got.Usable() {
		t.Fatalf("%+v признана непригодной из-за незнакомого ключа", got)
	}
}
