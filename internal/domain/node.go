package domain

// maxPort — верхняя граница TCP-порта.
const maxPort = 65535

// NodeAgent — как backend достаёт и аутентифицирует агента ноды (§6, §14).
// Клиенту эти параметры не показываются никогда.
type NodeAgent struct {
	Endpoint            string
	TLSServerName       string
	CertificateIdentity string
}

// NodePublic — публичные параметры ноды: блок node.public манифеста (§6) плюс её
// display_name, который в манифесте лежит уровнем выше, рядом с node_id.
//
// Manifest является authority этих значений (§17), поэтому тип живёт в домене, а
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
	DisplayName      string
}

// Usable сообщает, что параметров хватает на рабочую VLESS URI (§8).
//
// Это НЕ проверка правил §6 — та строже и живёт в ValidateManifest: она требует
// ровно tcp и xtls-rprx-vision и проверяет форму fingerprint. Здесь же вопрос
// другой: можно ли вообще собрать ссылку из того, что уже лежит в проекции.
// Read-путь не переспрашивает бизнес-правила приёма — иначе смягчение §6 в
// будущей версии молча погасило бы ссылки, выданные по прежним правилам.
//
// ShortID в обязательные не входит: пустой sid — легальная конфигурация REALITY,
// и требовать его значило бы объявить рабочую ноду сломанной. DisplayName тоже:
// пустой фрагмент URI безвреден.
func (n NodePublic) Usable() bool {
	return n.Address != "" &&
		n.Port > 0 && n.Port <= maxPort &&
		n.RealityPublicKey != "" &&
		n.ServerName != "" &&
		n.Fingerprint != "" &&
		n.Transport != "" &&
		n.Flow != ""
}
