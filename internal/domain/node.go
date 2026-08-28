package domain

// maxPort — верхняя граница TCP-порта.
const maxPort = 65535

// NodeAgent — как backend достаёт и аутентифицирует агента ноды.
// Клиенту эти параметры не показываются никогда.
type NodeAgent struct {
	Endpoint            string
	TLSServerName       string
	CertificateIdentity string
}

// NodePublic — публичные параметры ноды: блок node.public манифеста плюс её
// display_name, который в манифесте лежит уровнем выше, рядом с node_id.
//
// Manifest является authority этих значений, поэтому тип живёт в домене, а
// не рядом со сборщиком URI: их пишет приём манифеста и читает Customer API, и
// две копии структуры разъехались бы.
//
// Транспорт и flow хранятся значениями, а не константами: manifest передаёт их
// явно, и проверка допустимого набора принадлежит его приёму, а не потребителям.
type NodePublic struct {
	Address          string
	Port             int
	RealityPublicKey string
	ServerName       string
	ShortID          string
	Fingerprint      string
	Transport        string
	Flow             string
	XHTTP            *XHTTPConfig
	DisplayName      string
}

// XHTTPConfig — параметры XHTTP, которые должны совпадать у Xray inbound и в
// клиентской VLESS URI. Nil означает, что транспорт ноды не XHTTP.
type XHTTPConfig struct {
	Path string
	Mode string
}

// Usable сообщает, что параметров хватает на рабочую VLESS URI.
//
// Это не проверка правил манифеста — та строже и живёт в ValidateManifest: она проверяет
// допустимый transport, требует xtls-rprx-vision и проверяет форму fingerprint. Здесь же вопрос
// другой: можно ли вообще собрать ссылку из того, что уже лежит в проекции.
// Read-путь не переспрашивает бизнес-правила приёма — иначе их смягчение в
// будущей версии молча погасило бы ссылки, выданные по прежним правилам. Для
// XHTTP дополнительно нужны path и mode.
//
// ShortID в обязательные не входит: пустой sid — легальная конфигурация REALITY,
// и с ним рабочая нода считалась бы сломанной. DisplayName тоже:
// пустой фрагмент URI безвреден.
func (n NodePublic) Usable() bool {
	baseUsable := n.Address != "" &&
		n.Port > 0 && n.Port <= maxPort &&
		n.RealityPublicKey != "" &&
		n.ServerName != "" &&
		n.Fingerprint != "" &&
		n.Transport != "" &&
		n.Flow != ""
	if !baseUsable {
		return false
	}
	if n.Transport == TransportXHTTP {
		return n.XHTTP != nil && n.XHTTP.Path != "" && n.XHTTP.Mode != ""
	}
	return true
}
