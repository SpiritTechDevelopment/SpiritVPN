package domain

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
)

// Канонизация снапшота и его digest (§6, решение 20).
//
// Digest считается из schema_version и отсортированного desired snapshot. В него
// НЕ входят:
//
//   - revision — иначе rollback перестал бы работать. §6 требует выполнять его
//     прежним desired snapshot под новой большей revision, и это возможно только
//     если один и тот же снапшот под разными номерами даёт один digest;
//   - allow_destructive — он request-scoped, действует на один вызов и желаемым
//     состоянием не является (§6).
//
// Детерминированный protobuf-марshal для этой роли не годится: его собственная
// документация не обещает стабильности между версиями библиотеки и языками, а
// digest обязан совпасть и через год, и в реализации CI на другом языке.
//
// Digest — SHA-256 от канонических байт, которые уходят в
// manifest_revisions.canonical_payload. ВНИМАНИЕ: колонка объявлена как jsonb
// (§11), а jsonb хранит разобранное значение, а не текст: PostgreSQL
// пересортировывает ключи объектов и нормализует пробелы. Побайтово сравнить
// сохранённое с пересчитанным средствами SQL поэтому нельзя. Логически значение
// сохраняется без потерь (дубликатов ключей и дробных чисел у нас нет), так что
// пересчёт digest из колонки возможен — но только через разбор в Go и повторную
// канонизацию, а не через sha256() прямо в запросе.

// canonicalManifest повторяет логическую структуру манифеста (§6), а не раскладку
// хранения: display_name остаётся рядом с node_id, как в самом манифесте.
// Привязывать digest к тому, как мы сегодня раскладываем jsonb, нельзя — смена
// раскладки обнулила бы все сохранённые digest.
//
// Ни одного map: порядок ключей в JSON задаётся порядком объявления полей, а
// обход map в Go недетерминирован.
type canonicalManifest struct {
	SchemaVersion uint32           `json:"schema_version"`
	Nodes         []canonicalNode  `json:"nodes"`
	Fleets        []canonicalFleet `json:"fleets"`
}

type canonicalNode struct {
	NodeID      string          `json:"node_id"`
	Agent       canonicalAgent  `json:"agent"`
	Public      canonicalPublic `json:"public"`
	DisplayName string          `json:"display_name"`
}

type canonicalAgent struct {
	Endpoint            string `json:"endpoint"`
	TLSServerName       string `json:"tls_server_name"`
	CertificateIdentity string `json:"certificate_identity"`
}

type canonicalPublic struct {
	Address          string `json:"address"`
	Port             int    `json:"port"`
	RealityPublicKey string `json:"reality_public_key"`
	ServerName       string `json:"server_name"`
	ShortID          string `json:"short_id"`
	Fingerprint      string `json:"fingerprint"`
	Transport        string `json:"transport"`
	Flow             string `json:"flow"`
}

type canonicalFleet struct {
	FleetID int64             `json:"vpn_fleet_id"`
	NodeIDs []string          `json:"node_ids"`
	Bridges []canonicalBridge `json:"bridges"`
}

type canonicalBridge struct {
	RoutingKey  string `json:"routing_key"`
	EntryNodeID string `json:"entry_node_id"`
	ExitNodeID  string `json:"exit_node_id"`
	EgressTag   string `json:"egress_tag"`
	DisplayName string `json:"display_name"`
}

// CanonicalizeManifest возвращает канонический payload снапшота и его digest.
//
// Ошибку не возвращает и вернуть не может: canonicalManifest состоит только из
// строк, чисел и срезов из них, а json.Marshal падает лишь на каналах, функциях,
// комплексных числах, циклах и чужом MarshalJSON. Ни того, ни другого в этом
// замкнутом наборе типов нет — поэтому и подпись без error, чтобы вызывающие не
// тащили через себя недостижимую ветку. Добавляя сюда поле, держите это свойство.
func CanonicalizeManifest(s ManifestSnapshot) (payload []byte, digest string) {
	payload, _ = json.Marshal(canonicalize(s))

	sum := sha256.Sum256(payload)
	return payload, hex.EncodeToString(sum[:])
}

// canonicalize приводит снапшот к канонической форме: сортирует всё, что в
// манифесте является множеством, и отбрасывает всё, что множеством не является.
//
// Сортировка обязана быть полной: два снапшота, отличающиеся только порядком нод
// в списке, описывают одну топологию и обязаны дать один digest — иначе повтор
// того же манифеста от CI с другим порядком обхода был бы отвергнут как конфликт.
func canonicalize(s ManifestSnapshot) canonicalManifest {
	canonical := canonicalManifest{
		SchemaVersion: s.SchemaVersion,
		Nodes:         make([]canonicalNode, 0, len(s.Nodes)),
		Fleets:        make([]canonicalFleet, 0, len(s.Fleets)),
	}

	for _, node := range s.Nodes {
		canonical.Nodes = append(canonical.Nodes, canonicalNode{
			NodeID: string(node.NodeID),
			Agent: canonicalAgent{
				Endpoint:            node.Agent.Endpoint,
				TLSServerName:       node.Agent.TLSServerName,
				CertificateIdentity: node.Agent.CertificateIdentity,
			},
			Public: canonicalPublic{
				Address:          node.Public.Address,
				Port:             node.Public.Port,
				RealityPublicKey: node.Public.RealityPublicKey,
				ServerName:       node.Public.ServerName,
				ShortID:          node.Public.ShortID,
				Fingerprint:      node.Public.Fingerprint,
				Transport:        node.Public.Transport,
				Flow:             node.Public.Flow,
			},
			DisplayName: node.Public.DisplayName,
		})
	}
	slices.SortFunc(canonical.Nodes, func(a, b canonicalNode) int {
		return cmp.Compare(a.NodeID, b.NodeID)
	})

	for _, fleet := range s.Fleets {
		canonical.Fleets = append(canonical.Fleets, canonicalFleet{
			FleetID: fleet.FleetID,
			NodeIDs: canonicalNodeIDs(fleet.NodeIDs),
			Bridges: canonicalBridges(fleet.Bridges),
		})
	}
	slices.SortFunc(canonical.Fleets, func(a, b canonicalFleet) int {
		return cmp.Compare(a.FleetID, b.FleetID)
	})

	return canonical
}

func canonicalNodeIDs(nodes []NodeID) []string {
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		ids = append(ids, string(node))
	}
	slices.Sort(ids)
	return ids
}

func canonicalBridges(bridges []ManifestBridge) []canonicalBridge {
	canonical := make([]canonicalBridge, 0, len(bridges))
	for _, bridge := range bridges {
		canonical = append(canonical, canonicalBridge{
			RoutingKey:  bridge.RoutingKey,
			EntryNodeID: string(bridge.EntryNodeID),
			ExitNodeID:  string(bridge.ExitNodeID),
			EgressTag:   bridge.EgressTag,
			DisplayName: bridge.DisplayName,
		})
	}
	slices.SortFunc(canonical, func(a, b canonicalBridge) int {
		return cmp.Compare(a.RoutingKey, b.RoutingKey)
	})
	return canonical
}
