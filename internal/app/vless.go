package app

import (
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
)

// Значения, фиксированные протоколом v1: в manifest они не передаются и от ноды
// не зависят.
const (
	vlessScheme     = "vless"
	vlessSecurity   = "reality"
	vlessEncryption = "none"
)

// BuildVLESSURI собирает готовую ссылку:
//
//	vless://<uuid>@<address>:<port>?security=reality&encryption=none&pbk=&fp=
//	    &type=&flow=&sni=&sid=#<url-encoded display_name>
//
// Результат нигде не хранится и живёт ровно столько, сколько строится ответ.
//
// ВНИМАНИЕ: возвращается обычная строка, и защита crypto.ClientUUID от попадания
// в лог на ней уже не работает — uuid внутри неё открыт. Единственный барьер —
// то, что тела ответов не логируются; на это есть регрессионный тест в
// grpcsvc.
func BuildVLESSURI(clientUUID crypto.ClientUUID, node domain.NodePublic, displayName string) string {
	uri := url.URL{
		Scheme: vlessScheme,
		User:   url.User(clientUUID.Reveal().String()),
		// JoinHostPort сам заключает IPv6-литерал в скобки; address может быть и
		// доменом, и адресом.
		Host:     net.JoinHostPort(node.Address, strconv.Itoa(node.Port)),
		RawQuery: vlessQuery(node),
		// Fragment, а не RawFragment: url.URL сам процентно кодирует его при
		// String(), в том числе non-ASCII display_name.
		Fragment: displayName,
	}
	return uri.String()
}

// vlessQuery собирает строку запроса в фиксированном порядке.
//
// Порядок параметров нормативен, поэтому url.Values.Encode() не подходит: он
// сортирует ключи по алфавиту. Пустые значения не опускаются — клиенты читают
// набор параметров позиционно-независимо, но их отсутствие и пустота для
// некоторых из них (sid) означают разное.
//
// Flow — исключение из этого правила: у XHTTP-ноды его нет вовсе, и ссылка
// повторяет конфигурацию ноды буквально, а не объявляет flow заданным и пустым.
// Форма TCP-ссылки при этом не меняется: там flow непуст всегда.
func vlessQuery(node domain.NodePublic) string {
	type queryParam struct{ key, value string }
	params := []queryParam{
		{"security", vlessSecurity},
		{"encryption", vlessEncryption},
		{"pbk", node.RealityPublicKey},
		{"fp", node.Fingerprint},
		{"type", node.Transport},
	}
	if node.Transport == domain.TransportXHTTP && node.XHTTP != nil {
		params = append(params,
			queryParam{"path", node.XHTTP.Path},
			queryParam{"mode", node.XHTTP.Mode},
		)
	}
	if node.Flow != "" {
		params = append(params, queryParam{"flow", node.Flow})
	}
	params = append(params,
		queryParam{"sni", node.ServerName},
		queryParam{"sid", node.ShortID},
	)

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
