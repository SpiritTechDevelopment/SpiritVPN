package postgres

import (
	"encoding/json"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
)

// nodePublicConfig — раскладка jsonb-колонки vpn_nodes.public_config.
//
// Ключи — блок node.public манифеста дословно, в snake_case (§6), плюс
// display_name, который в манифесте лежит уровнем выше, рядом с node_id: под него
// решено не заводить отдельную колонку (решение 16).
//
// Эта структура — единственное место, где формат колонки известен. Когда появится
// срез приёма манифеста, writer встанет сюда же, рядом с reader'ом: так формат
// физически не сможет разъехаться между тем, кто пишет, и тем, кто читает
// (решение 19). Схему внутри jsonb не версионируем — manifest уже версионирован
// целиком своим schema_version (§6).
type nodePublicConfig struct {
	Address          string `json:"address"`
	Port             int    `json:"port"`
	RealityPublicKey string `json:"reality_public_key"`
	ServerName       string `json:"server_name"`
	ShortID          string `json:"short_id"`
	Fingerprint      string `json:"fingerprint"`
	Transport        string `json:"transport"`
	Flow             string `json:"flow"`
	DisplayName      string `json:"display_name"`
}

// nodePublicFrom разбирает колонку в доменную форму.
//
// Ошибка разбора не возвращается: нераспознанный jsonb даёт нулевую структуру,
// которая не проходит app.NodePublic.Usable, и ссылка на этой ноде уходит наружу
// как FAILED (решение 18). Отдельный канал для причины не заводится — исход у
// «json битый» и «поля пустые» один и тот же, а различать их будет метрика §15.
func nodePublicFrom(raw []byte) app.NodePublic {
	var config nodePublicConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		return app.NodePublic{}
	}

	return app.NodePublic{
		Address:          config.Address,
		Port:             config.Port,
		RealityPublicKey: config.RealityPublicKey,
		ServerName:       config.ServerName,
		ShortID:          config.ShortID,
		Fingerprint:      config.Fingerprint,
		Transport:        config.Transport,
		Flow:             config.Flow,
		DisplayName:      config.DisplayName,
	}
}
