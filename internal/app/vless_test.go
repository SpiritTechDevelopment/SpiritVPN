package app_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
)

// testClientUUID — фиксированное значение, чтобы ожидаемая URI была буквальной.
var testClientUUID = crypto.NewClientUUID(uuid.MustParse("f81d4fae-7dec-11d0-a765-00a0c91e6bf6"))

// testNode — нода из примера manifest §6.
func testNode() domain.NodePublic {
	return domain.NodePublic{
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
}

// TestBuildVLESSURIMatchesSpec пиннит форму URI из §8 буквально, вместе с
// порядком параметров: он нормативен, и url.Values.Encode() его бы нарушил,
// отсортировав ключи по алфавиту.
func TestBuildVLESSURIMatchesSpec(t *testing.T) {
	const want = "vless://f81d4fae-7dec-11d0-a765-00a0c91e6bf6@nl.example.com:443" +
		"?security=reality&encryption=none&pbk=pub-key&fp=chrome&type=tcp" +
		"&flow=xtls-rprx-vision&sni=www.example.org&sid=ab12" +
		"#Netherlands"

	got := app.BuildVLESSURI(testClientUUID, testNode(), testNode().DisplayName)
	if got != want {
		t.Fatalf("URI\n  получено %q\n  ожидалось %q", got, want)
	}
}

// TestBuildVLESSURIKeepsEmptyShortID — пустой sid остаётся в строке параметром с
// пустым значением, а не исчезает: отсутствие ключа и пустое значение читаются
// клиентами по-разному.
func TestBuildVLESSURIKeepsEmptyShortID(t *testing.T) {
	node := testNode()
	node.ShortID = ""

	got := app.BuildVLESSURI(testClientUUID, node, node.DisplayName)
	if !strings.Contains(got, "&sid=") {
		t.Fatalf("URI %q потеряла параметр sid", got)
	}
}

// TestBuildVLESSURIEncodesFragment — §8 требует url-encoded display_name.
// Фрагмент разбирается обратно и обязан совпасть с исходным именем.
func TestBuildVLESSURIEncodesFragment(t *testing.T) {
	names := []string{
		"Netherlands via Germany",
		"Нидерланды через Германию",
		"NL #1 / DE",
		"",
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			raw := app.BuildVLESSURI(testClientUUID, testNode(), name)

			if strings.ContainsAny(raw, " \n\t") {
				t.Fatalf("URI %q содержит непроцентированный пробельный символ", raw)
			}

			parsed, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("URI %q не разбирается: %v", raw, err)
			}
			if parsed.Fragment != name {
				t.Fatalf("фрагмент %q, ожидался %q (URI %q)", parsed.Fragment, name, raw)
			}
		})
	}
}

// TestBuildVLESSURIBracketsIPv6 — address может быть и доменом, и адресом (§6).
// Голый IPv6-литерал сделал бы URI неразбираемой.
func TestBuildVLESSURIBracketsIPv6(t *testing.T) {
	node := testNode()
	node.Address = "2001:db8::1"

	raw := app.BuildVLESSURI(testClientUUID, node, node.DisplayName)

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("URI %q не разбирается: %v", raw, err)
	}
	if got := parsed.Host; got != "[2001:db8::1]:443" {
		t.Fatalf("host %q, ожидался [2001:db8::1]:443", got)
	}
}

// TestBuildVLESSURICarriesCredential — uuid уезжает в user info в открытом виде:
// на готовой строке редакция crypto.ClientUUID уже не работает, и это ровно то,
// ради чего ответ не логируется и не кешируется (§8).
func TestBuildVLESSURICarriesCredential(t *testing.T) {
	raw := app.BuildVLESSURI(testClientUUID, testNode(), "NL")

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("URI %q не разбирается: %v", raw, err)
	}
	if got := parsed.User.Username(); got != testClientUUID.Reveal().String() {
		t.Fatalf("uuid %q, ожидался %q", got, testClientUUID.Reveal())
	}
	if _, hasPassword := parsed.User.Password(); hasPassword {
		t.Fatal("в user info попал пароль, ожидался только uuid")
	}
}

// TestNodePublicUsable — §6 валидирует параметры до записи, поэтому непригодная
// нода означает рассогласованную проекцию. Список обязательных полей здесь и
// есть определение «пригодна» (решение 18).
func TestNodePublicUsable(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*domain.NodePublic)
		want   bool
	}{
		{"полный набор", func(*domain.NodePublic) {}, true},

		{"пустой sid допустим", func(n *domain.NodePublic) { n.ShortID = "" }, true},
		{"пустое display_name допустимо", func(n *domain.NodePublic) { n.DisplayName = "" }, true},

		{"нет адреса", func(n *domain.NodePublic) { n.Address = "" }, false},
		{"нулевой порт", func(n *domain.NodePublic) { n.Port = 0 }, false},
		{"отрицательный порт", func(n *domain.NodePublic) { n.Port = -1 }, false},
		{"порт за границей диапазона", func(n *domain.NodePublic) { n.Port = 65536 }, false},
		{"нет reality public key", func(n *domain.NodePublic) { n.RealityPublicKey = "" }, false},
		{"нет server name", func(n *domain.NodePublic) { n.ServerName = "" }, false},
		{"нет fingerprint", func(n *domain.NodePublic) { n.Fingerprint = "" }, false},
		{"нет transport", func(n *domain.NodePublic) { n.Transport = "" }, false},
		{"нет flow", func(n *domain.NodePublic) { n.Flow = "" }, false},

		{"пустая структура", func(n *domain.NodePublic) { *n = domain.NodePublic{} }, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := testNode()
			tt.mutate(&node)

			if got := node.Usable(); got != tt.want {
				t.Fatalf("Usable() = %v, ожидалось %v (%+v)", got, tt.want, node)
			}
		})
	}
}
