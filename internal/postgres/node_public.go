package postgres

import (
	"encoding/json"

	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	"github.com/RomanRyabinkin/SpiritVPN/internal/nodeagent"
)

// nodePublicConfig — раскладка jsonb-колонки vpn_nodes.public_config.
//
// Ключи — блок node.public манифеста дословно, в snake_case, плюс
// display_name, который в манифесте лежит уровнем выше, рядом с node_id: под него
// решено не заводить отдельную колонку.
//
// Эта структура — единственное место, где формат колонки известен. Когда появится
// срез приёма манифеста, writer встанет сюда же, рядом с reader'ом: так формат
// физически не сможет разъехаться между тем, кто пишет, и тем, кто читает.
// Схему внутри jsonb не версионируем — manifest уже версионирован
// целиком своим schema_version.
type nodePublicConfig struct {
	Address          string                 `json:"address"`
	Port             int                    `json:"port"`
	RealityPublicKey string                 `json:"reality_public_key"`
	ServerName       string                 `json:"server_name"`
	ShortID          string                 `json:"short_id"`
	Fingerprint      string                 `json:"fingerprint"`
	Transport        string                 `json:"transport"`
	Flow             string                 `json:"flow"`
	XHTTP            *nodePublicXHTTPConfig `json:"xhttp,omitempty"`
	DisplayName      string                 `json:"display_name"`
}

type nodePublicXHTTPConfig struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
}

// nodeAgentConfig — раскладка jsonb-колонки vpn_nodes.agent_config: блок
// node.agent манифеста дословно. Клиенту эти значения не показываются
// никогда, и в VLESS URI они не попадают.
type nodeAgentConfig struct {
	Endpoint            string `json:"endpoint"`
	TLSServerName       string `json:"tls_server_name"`
	CertificateIdentity string `json:"certificate_identity"`
}

// nodeConfigJSON — writer, парный к nodePublicFrom.
//
// Стоит рядом с reader'ом намеренно: формат колонок известен ровно этому файлу,
// и разъехаться тому, кто пишет, с тем, кто читает, физически негде.
//
// Ошибка сериализации не возвращается и возникнуть не может: обе структуры
// состоят только из строк и чисел (см. довод у CanonicalizeManifest).
func nodeConfigJSON(node domain.ManifestNode) (agent, public []byte) {
	agent, _ = json.Marshal(nodeAgentConfig{
		Endpoint:            node.Agent.Endpoint,
		TLSServerName:       node.Agent.TLSServerName,
		CertificateIdentity: node.Agent.CertificateIdentity,
	})

	var xhttp *nodePublicXHTTPConfig
	if node.Public.XHTTP != nil {
		xhttp = &nodePublicXHTTPConfig{
			Path: node.Public.XHTTP.Path,
			Mode: node.Public.XHTTP.Mode,
		}
	}

	public, _ = json.Marshal(nodePublicConfig{
		Address:          node.Public.Address,
		Port:             node.Public.Port,
		RealityPublicKey: node.Public.RealityPublicKey,
		ServerName:       node.Public.ServerName,
		ShortID:          node.Public.ShortID,
		Fingerprint:      node.Public.Fingerprint,
		Transport:        node.Public.Transport,
		Flow:             node.Public.Flow,
		XHTTP:            xhttp,
		DisplayName:      node.Public.DisplayName,
	})

	return agent, public
}

// nodeAgentFrom разбирает agent_config в endpoint вызова агента.
//
// Стоит рядом с writer'ом по той же причине, что и nodePublicFrom: формат колонки
// известен ровно этому файлу.
//
// Ошибка разбора не возвращается: нераспознанный jsonb даёт нулевую структуру,
// которую отвергнет клиент агента — с исходом retryable и alert.
// Второй канал для той же причины не нужен, исход у «json битый» и «поля пустые»
// один и тот же.
func nodeAgentFrom(nodeID string, raw []byte) nodeagent.Endpoint {
	var config nodeAgentConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		return nodeagent.Endpoint{NodeID: nodeID}
	}

	return nodeagent.Endpoint{
		NodeID:              nodeID,
		Address:             config.Endpoint,
		TLSServerName:       config.TLSServerName,
		CertificateIdentity: config.CertificateIdentity,
	}
}

// nodePublicFrom разбирает колонку в доменную форму.
//
// Ошибка разбора не возвращается: нераспознанный jsonb даёт нулевую структуру,
// которая не проходит domain.NodePublic.Usable, и ссылка на этой ноде уходит наружу
// как FAILED. Отдельный канал для причины не заводится — исход у
// «json битый» и «поля пустые» один и тот же, а различать их будет метрика.
func nodePublicFrom(raw []byte) domain.NodePublic {
	var config nodePublicConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		return domain.NodePublic{}
	}

	var xhttp *domain.XHTTPConfig
	if config.XHTTP != nil {
		xhttp = &domain.XHTTPConfig{
			Path: config.XHTTP.Path,
			Mode: config.XHTTP.Mode,
		}
	}

	return domain.NodePublic{
		Address:          config.Address,
		Port:             config.Port,
		RealityPublicKey: config.RealityPublicKey,
		ServerName:       config.ServerName,
		ShortID:          config.ShortID,
		Fingerprint:      config.Fingerprint,
		Transport:        config.Transport,
		Flow:             config.Flow,
		XHTTP:            xhttp,
		DisplayName:      config.DisplayName,
	}
}
