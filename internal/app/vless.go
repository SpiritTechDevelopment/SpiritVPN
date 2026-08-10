package app

import (
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
)

// Значения, фиксированные протоколом v1: в manifest они не передаются и от ноды
// не зависят (§6, §8).
const (
	vlessScheme     = "vless"
	vlessSecurity   = "reality"
	vlessEncryption = "none"
)

// maxPort — верхняя граница TCP-порта.
const maxPort = 65535

// NodePublic — публичные параметры входной ноды, то есть блок node.public
// manifest (§6) плюс её display_name.
//
// Всё, кроме uuid, в URI берётся отсюда (§8). Транспорт и flow в v1 всегда
// tcp/xtls-rprx-vision, но хранятся как значения, а не как константы: manifest
// передаёт их явно, а валидация допустимого набора принадлежит его приёму (§6),
// не сборщику URI.
type NodePublic struct {
	Address          string
	Port             int
	RealityPublicKey string
	ServerName       string
	ShortID          string
	Fingerprint      string
	Transport        string
	Flow             string
	// DisplayName — человекочитаемое имя самой ноды. Для FREEDOM оно уходит во
	// фрагмент URI; для BRIDGE фрагмент берётся из display_name связи (§8).
	DisplayName string
}

// Usable сообщает, что параметров хватает на рабочую URI.
//
// §6 валидирует их до записи, поэтому непригодные параметры означают
// рассогласованную проекцию, а не ошибку вызывающего; наружу это уходит как
// FAILED одной ссылки (решение 18).
//
// ShortID в список обязательных не входит: пустой sid — легальная конфигурация
// REALITY, и требовать его значило бы объявить рабочую ноду сломанной.
// DisplayName тоже необязателен: пустой фрагмент URI безвреден.
func (n NodePublic) Usable() bool {
	return n.Address != "" &&
		n.Port > 0 && n.Port <= maxPort &&
		n.RealityPublicKey != "" &&
		n.ServerName != "" &&
		n.Fingerprint != "" &&
		n.Transport != "" &&
		n.Flow != ""
}

// BuildVLESSURI собирает готовую ссылку (§8):
//
//	vless://<uuid>@<address>:<port>?security=reality&encryption=none&pbk=&fp=
//	    &type=&flow=&sni=&sid=#<url-encoded display_name>
//
// Результат нигде не хранится и живёт ровно столько, сколько строится ответ.
//
// ВНИМАНИЕ: возвращается обычная строка, и защита crypto.ClientUUID от попадания
// в лог на ней уже не работает — uuid внутри неё открыт. Единственный барьер —
// то, что тела ответов не логируются (§8, §15); на это есть регрессионный тест в
// grpcsvc.
func BuildVLESSURI(clientUUID crypto.ClientUUID, node NodePublic, displayName string) string {
	uri := url.URL{
		Scheme: vlessScheme,
		User:   url.User(clientUUID.Reveal().String()),
		// JoinHostPort сам заключает IPv6-литерал в скобки; address может быть и
		// доменом, и адресом (§6).
		Host:     net.JoinHostPort(node.Address, strconv.Itoa(node.Port)),
		RawQuery: vlessQuery(node),
		// Fragment, а не RawFragment: url.URL сам процентно кодирует его при
		// String(), в том числе non-ASCII display_name.
		Fragment: displayName,
	}
	return uri.String()
}

// vlessQuery собирает строку запроса в порядке §8.
//
// Порядок параметров нормативен, поэтому url.Values.Encode() не подходит: он
// сортирует ключи по алфавиту. Пустые значения не опускаются — клиенты читают
// набор параметров позиционно-независимо, но их отсутствие и пустота для
// некоторых из них (sid) означают разное.
func vlessQuery(node NodePublic) string {
	params := []struct{ key, value string }{
		{"security", vlessSecurity},
		{"encryption", vlessEncryption},
		{"pbk", node.RealityPublicKey},
		{"fp", node.Fingerprint},
		{"type", node.Transport},
		{"flow", node.Flow},
		{"sni", node.ServerName},
		{"sid", node.ShortID},
	}

	var query strings.Builder
	for i, param := range params {
		if i > 0 {
			query.WriteByte('&')
		}
		query.WriteString(param.key)
		query.WriteByte('=')
		query.WriteString(url.QueryEscape(param.value))
	}
	return query.String()
}
