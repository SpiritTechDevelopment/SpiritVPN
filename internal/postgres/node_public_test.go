package postgres

import (
	"encoding/json"
	"testing"

	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
)

// TestNodePublicFromPinsColumnFormat пиннит раскладку public_config.
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

	want := domain.NodePublic{
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

func TestNodePublicFromReadsXHTTP(t *testing.T) {
	const raw = `{
	  "address": "nl.example.com",
	  "port": 443,
	  "reality_public_key": "pub-key",
	  "server_name": "www.example.org",
	  "short_id": "ab12",
	  "fingerprint": "firefox",
	  "transport": "xhttp",
	  "flow": "xtls-rprx-vision",
	  "xhttp": {"path": "/api/v1/connect", "mode": "auto"},
	  "display_name": "Netherlands"
	}`

	got := nodePublicFrom([]byte(raw))
	if got.XHTTP == nil || got.XHTTP.Path != "/api/v1/connect" || got.XHTTP.Mode != "auto" {
		t.Fatalf("XHTTP-настройки потеряны: %+v", got)
	}
	if !got.Usable() {
		t.Fatalf("XHTTP-нода признана непригодной: %+v", got)
	}
}

func TestNodeConfigJSONWritesXHTTP(t *testing.T) {
	node := domain.ManifestNode{Public: domain.NodePublic{
		Transport: domain.TransportXHTTP,
		XHTTP: &domain.XHTTPConfig{
			Path: "/api/v1/connect",
			Mode: "auto",
		},
	}}

	_, raw := nodeConfigJSON(node)
	var config nodePublicConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatalf("разбор записанного JSON: %v", err)
	}
	if config.XHTTP == nil || config.XHTTP.Path != "/api/v1/connect" || config.XHTTP.Mode != "auto" {
		t.Fatalf("XHTTP-настройки потеряны при записи: %s", raw)
	}
}

// TestNodePublicFromDegradesOnBadInput — нераспознанный jsonb не
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
