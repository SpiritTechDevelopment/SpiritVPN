package domain

import (
	"net"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// ManifestSchemaVersion — актуальная версия схемы манифеста. Backend также
// принимает v1, чтобы обновление не требовало синхронного переключения infra.
const (
	ManifestSchemaVersionV1 = 1
	ManifestSchemaVersion   = 2
)

// Значения transport и flow, поддерживаемые версиями манифеста. Другие
// отклоняются на приёме; потребители проекции их не перепроверяют.
const (
	TransportTCP       = "tcp"
	TransportXHTTP     = "xhttp"
	FlowXTLSRprxVision = "xtls-rprx-vision"
)

const maxXHTTPPathBytes = 256

var supportedXHTTPModes = map[string]struct{}{
	"auto":       {},
	"packet-up":  {},
	"stream-up":  {},
	"stream-one": {},
}

// Лимиты размера манифеста. Константы, а не конфигурация: ручка без потребителя — это ещё один
// способ выкатить непроверенную нагрузкой топологию. Лимит самого
// RPC в 4 MiB отдельно не задаётся — он совпадает с дефолтом gRPC.
const (
	MaxManifestFleets    = 100
	MaxManifestNodes     = 100
	MaxManifestRelations = 900
	MaxNodesPerFleet     = 10
)

// fingerprintPattern — ASCII-токен длиной 1–64 из [A-Za-z0-9._-]. Значение
// backend не преобразует, а кладёт в параметр fp URI как есть, поэтому проверять
// его надо здесь, до записи.
var fingerprintPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ManifestNode — одна логическая нода снапшота.
type ManifestNode struct {
	NodeID NodeID
	Agent  NodeAgent
	// Public включает display_name: в манифесте он лежит рядом с node_id, но
	// хранится и читается вместе с публичными параметрами.
	Public NodePublic
}

// ManifestBridge — направленная связь внутри fleet.
type ManifestBridge struct {
	RoutingKey  string
	EntryNodeID NodeID
	ExitNodeID  NodeID
	EgressTag   string
	DisplayName string
}

// ManifestFleet — один fleet снапшота.
type ManifestFleet struct {
	FleetID int64
	NodeIDs []NodeID
	Bridges []ManifestBridge
}

// ManifestSnapshot — полный versioned snapshot топологии.
//
// allow_destructive сюда не входит: он request-scoped, действует только на один
// вызов и в желаемое состояние не превращается. Из-за этого он не участвует и в
// каноническом digest.
type ManifestSnapshot struct {
	SchemaVersion uint32
	Revision      int64
	Nodes         []ManifestNode
	Fleets        []ManifestFleet
}

// ValidateManifest проверяет снапшот сам по себе.
//
// Только те правила, которым не нужно принятое состояние: форма, лимиты,
// уникальность внутри снапшота и ссылочная целостность. Правила, сравнивающие
// снапшот с уже принятым (монотонность revision, digest, пропавший fleet,
// destructive), живут в PlanManifest — иначе половина из них потребовала бы
// таскать сюда проекцию, и разделение «чистая проверка / решение по состоянию»
// перестало бы читаться.
//
// Ошибки несут деталь из запроса: см. ManifestValidationError.
func ValidateManifest(s ManifestSnapshot) error {
	if s.SchemaVersion != ManifestSchemaVersionV1 && s.SchemaVersion != ManifestSchemaVersion {
		return manifestError(ErrManifestSchemaVersion, "получена %d, поддерживаются %d и %d",
			s.SchemaVersion, ManifestSchemaVersionV1, ManifestSchemaVersion)
	}
	// Revision здесь int64, а не uint64 с провода: колонка manifest_revisions.revision
	// объявлена как bigint, и сужение закрепляется на границе gRPC вместе с проверкой
	// диапазона — тем же приёмом, что и секундная точность expires_at.
	// Неположительное значение сюда дойти не должно, но проверка оставлена: молча
	// принятая отрицательная revision сломала бы монотонность навсегда.
	if s.Revision <= 0 {
		return manifestError(ErrManifestRevisionInvalid, "получена %d", s.Revision)
	}

	if err := validateManifestSize(s); err != nil {
		return err
	}

	known, err := validateManifestNodes(s.Nodes, s.SchemaVersion)
	if err != nil {
		return err
	}

	return validateManifestFleets(s.Fleets, known)
}

// validateManifestSize проверяет лимиты размера до всего остального: разбирать по
// косточкам снапшот, который заведомо не будет принят, не нужно.
func validateManifestSize(s ManifestSnapshot) error {
	if len(s.Nodes) > MaxManifestNodes {
		return manifestError(ErrManifestTooLarge, "нод %d, предел %d", len(s.Nodes), MaxManifestNodes)
	}
	if len(s.Fleets) > MaxManifestFleets {
		return manifestError(ErrManifestTooLarge, "fleets %d, предел %d", len(s.Fleets), MaxManifestFleets)
	}

	relations := 0
	for _, fleet := range s.Fleets {
		if len(fleet.NodeIDs) > MaxNodesPerFleet {
			return manifestError(ErrManifestTooLarge, "fleet %d: нод %d, предел %d",
				fleet.FleetID, len(fleet.NodeIDs), MaxNodesPerFleet)
		}
		relations += len(fleet.Bridges)
	}
	if relations > MaxManifestRelations {
		return manifestError(ErrManifestTooLarge, "связей %d, предел %d", relations, MaxManifestRelations)
	}

	return nil
}

// validateManifestNodes проверяет ноды и возвращает множество известных node_id,
// против которого дальше сверяются все ссылки.
func validateManifestNodes(nodes []ManifestNode, schemaVersion uint32) (map[NodeID]struct{}, error) {
	known := make(map[NodeID]struct{}, len(nodes))

	for _, node := range nodes {
		if node.NodeID == "" {
			return nil, manifestError(ErrManifestNodeInvalid, "пустой node_id")
		}
		if _, duplicate := known[node.NodeID]; duplicate {
			return nil, manifestError(ErrManifestDuplicate, "node_id %s", node.NodeID)
		}
		known[node.NodeID] = struct{}{}

		if err := validateNodeAgent(node); err != nil {
			return nil, err
		}
		if err := validateNodePublic(node, schemaVersion); err != nil {
			return nil, err
		}
	}

	return known, nil
}

// validateNodeAgent — endpoint и TLS identity валидируются до записи. Без них
// dispatcher не сможет ни дозвониться до агента, ни проверить, что говорит именно
// с ним.
func validateNodeAgent(node ManifestNode) error {
	if node.Agent.TLSServerName == "" {
		return manifestError(ErrManifestNodeInvalid, "нода %s: пустой tls_server_name", node.NodeID)
	}
	if node.Agent.CertificateIdentity == "" {
		return manifestError(ErrManifestNodeInvalid, "нода %s: пустой certificate_identity", node.NodeID)
	}

	// Проверяется разбираемость, а не достижимость: backend не ходит в сеть на
	// приёме манифеста. Нераспознанный endpoint иначе всплыл бы только при первой
	// операции, уже после commit топологии.
	host, port, err := net.SplitHostPort(node.Agent.Endpoint)
	if err != nil {
		return manifestError(ErrManifestNodeInvalid, "нода %s: endpoint не имеет вида host:port", node.NodeID)
	}
	if host == "" {
		return manifestError(ErrManifestNodeInvalid, "нода %s: пустой host в endpoint", node.NodeID)
	}
	number, err := strconv.Atoi(port)
	if err != nil || number <= 0 || number > maxPort {
		return manifestError(ErrManifestNodeInvalid, "нода %s: недопустимый порт endpoint %q", node.NodeID, port)
	}

	return nil
}

// validateNodePublic — обязательные REALITY-поля, поддерживаемые значения
// transport и совместимый с ним flow.
//
// Строже, чем NodePublic.Usable: тот отвечает на вопрос «соберётся ли URI» и
// применяется к тому, что уже лежит в проекции. Здесь же решается, пускать ли
// значение в проекцию вообще.
func validateNodePublic(node ManifestNode, schemaVersion uint32) error {
	public := node.Public

	switch {
	case public.Address == "":
		return manifestError(ErrManifestNodeInvalid, "нода %s: пустой address", node.NodeID)
	case public.Port <= 0 || public.Port > maxPort:
		return manifestError(ErrManifestNodeInvalid, "нода %s: порт %d вне диапазона 1..%d",
			node.NodeID, public.Port, maxPort)
	case public.RealityPublicKey == "":
		return manifestError(ErrManifestNodeInvalid, "нода %s: пустой reality_public_key", node.NodeID)
	case public.ServerName == "":
		return manifestError(ErrManifestNodeInvalid, "нода %s: пустой server_name", node.NodeID)
	case !fingerprintPattern.MatchString(public.Fingerprint):
		return manifestError(ErrManifestNodeInvalid,
			"нода %s: fingerprint %q не является ASCII-токеном 1..64 из [A-Za-z0-9._-]",
			node.NodeID, public.Fingerprint)
	}

	if schemaVersion == ManifestSchemaVersionV1 {
		if public.Transport != TransportTCP {
			return manifestError(ErrManifestNodeInvalid, "нода %s: transport %q, v1 поддерживает только %q",
				node.NodeID, public.Transport, TransportTCP)
		}
		if public.XHTTP != nil {
			return manifestError(ErrManifestNodeInvalid, "нода %s: xhttp недопустим для transport %q в v1",
				node.NodeID, public.Transport)
		}
		return validateFlow(node.NodeID, public.Transport, public.Flow)
	}

	switch public.Transport {
	case TransportTCP:
		if public.XHTTP != nil {
			return manifestError(ErrManifestNodeInvalid, "нода %s: xhttp допустим только для transport %q",
				node.NodeID, TransportXHTTP)
		}
	case TransportXHTTP:
		if err := validateXHTTP(node.NodeID, public.XHTTP); err != nil {
			return err
		}
	default:
		return manifestError(ErrManifestNodeInvalid, "нода %s: неподдерживаемый transport %q",
			node.NodeID, public.Transport)
	}

	return validateFlow(node.NodeID, public.Transport, public.Flow)
}

// validateFlow — flow задаётся транспортом, а не выбирается независимо от него.
//
// Vision работает только там, где VLESS дотягивается до соединения напрямую:
// tls.Conn, reality.Conn либо VLESS Encryption. XHTTP заворачивает поток в HTTP,
// и Xray отвечает "XTLS only supports TLS and REALITY directly for now."
// Отказ при этом происходит на стороне клиента, до подключения к ноде, поэтому
// ни агент, ни мониторинг ноды такую ссылку не увидят вовсе: единственное место,
// где несовместимость ещё можно заметить, — этот приём.
func validateFlow(nodeID NodeID, transport, flow string) error {
	if transport == TransportXHTTP {
		if flow != "" {
			return manifestError(ErrManifestNodeInvalid,
				"нода %s: flow %q, для transport %q flow обязан быть пустым",
				nodeID, flow, TransportXHTTP)
		}
		return nil
	}

	if flow != FlowXTLSRprxVision {
		return manifestError(ErrManifestNodeInvalid,
			"нода %s: flow %q, для transport %q поддерживается только %q",
			nodeID, flow, transport, FlowXTLSRprxVision)
	}

	return nil
}

func validateXHTTP(nodeID NodeID, config *XHTTPConfig) error {
	if config == nil {
		return manifestError(ErrManifestNodeInvalid, "нода %s: для transport %q обязателен xhttp",
			nodeID, TransportXHTTP)
	}
	if config.Path == "" || !strings.HasPrefix(config.Path, "/") {
		return manifestError(ErrManifestNodeInvalid, "нода %s: xhttp.path должен начинаться с '/'",
			nodeID)
	}
	if len(config.Path) > maxXHTTPPathBytes {
		return manifestError(ErrManifestNodeInvalid, "нода %s: xhttp.path длиннее %d байт",
			nodeID, maxXHTTPPathBytes)
	}
	if strings.ContainsAny(config.Path, "?#") ||
		strings.IndexFunc(config.Path, unicode.IsSpace) >= 0 ||
		strings.IndexFunc(config.Path, unicode.IsControl) >= 0 {
		return manifestError(ErrManifestNodeInvalid,
			"нода %s: xhttp.path содержит пробел, управляющий символ, query или fragment", nodeID)
	}
	if _, ok := supportedXHTTPModes[config.Mode]; !ok {
		return manifestError(ErrManifestNodeInvalid, "нода %s: неподдерживаемый xhttp.mode %q",
			nodeID, config.Mode)
	}
	return nil
}

// validateManifestFleets проверяет fleets и их связи против известных нод.
func validateManifestFleets(fleets []ManifestFleet, known map[NodeID]struct{}) error {
	seenFleets := make(map[int64]struct{}, len(fleets))

	for _, fleet := range fleets {
		if fleet.FleetID <= 0 {
			return manifestError(ErrManifestDuplicate, "vpn_fleet_id %d должен быть > 0", fleet.FleetID)
		}
		if _, duplicate := seenFleets[fleet.FleetID]; duplicate {
			return manifestError(ErrManifestDuplicate, "vpn_fleet_id %d", fleet.FleetID)
		}
		seenFleets[fleet.FleetID] = struct{}{}

		members, err := validateFleetMembers(fleet, known)
		if err != nil {
			return err
		}
		if err := validateFleetBridges(fleet, known, members); err != nil {
			return err
		}
	}

	return nil
}

// validateFleetMembers возвращает состав fleet, против которого проверяются
// концы связей.
func validateFleetMembers(fleet ManifestFleet, known map[NodeID]struct{}) (map[NodeID]struct{}, error) {
	members := make(map[NodeID]struct{}, len(fleet.NodeIDs))

	for _, nodeID := range fleet.NodeIDs {
		if _, ok := known[nodeID]; !ok {
			return nil, manifestError(ErrManifestUnknownNode, "fleet %d ссылается на ноду %s",
				fleet.FleetID, nodeID)
		}
		if _, duplicate := members[nodeID]; duplicate {
			return nil, manifestError(ErrManifestDuplicate, "fleet %d: нода %s указана дважды",
				fleet.FleetID, nodeID)
		}
		members[nodeID] = struct{}{}
	}

	return members, nil
}

// pairKey — направленная пара концов связи; ключ уникальности.
type pairKey struct {
	entry NodeID
	exit  NodeID
}

// validateFleetBridges — routing_key и пара (entry, exit) уникальны внутри
// fleet, BRIDGE и EXIT различны и входят в тот же fleet, egress_tag непуст.
//
// Уникальность пары — правило только про снапшот, хотя выглядит как правило про
// хранимое состояние. Удалённая связь остаётся строкой с current = false, но
// уникальным индексом не учитывается, поэтому освободившуюся пару может занять
// новый routing_key. Это и есть перенос route.
//
// А неизменяемость пары у существующего routing_key сравнивает снапшот с
// принятым состоянием и потому живёт в PlanManifest.
func validateFleetBridges(fleet ManifestFleet, known, members map[NodeID]struct{}) error {
	seenKeys := make(map[string]struct{}, len(fleet.Bridges))
	seenPairs := make(map[pairKey]string, len(fleet.Bridges))

	for _, bridge := range fleet.Bridges {
		if bridge.RoutingKey == "" {
			return manifestError(ErrManifestBridgeInvalid, "fleet %d: пустой routing_key", fleet.FleetID)
		}
		if _, duplicate := seenKeys[bridge.RoutingKey]; duplicate {
			return manifestError(ErrManifestDuplicate, "fleet %d: routing_key %s",
				fleet.FleetID, bridge.RoutingKey)
		}
		seenKeys[bridge.RoutingKey] = struct{}{}

		if bridge.EgressTag == "" {
			return manifestError(ErrManifestBridgeInvalid, "связь %d/%s: пустой egress_tag",
				fleet.FleetID, bridge.RoutingKey)
		}
		if bridge.EntryNodeID == bridge.ExitNodeID {
			return manifestError(ErrManifestBridgeInvalid, "связь %d/%s: entry совпадает с exit (%s)",
				fleet.FleetID, bridge.RoutingKey, bridge.EntryNodeID)
		}

		for _, end := range []NodeID{bridge.EntryNodeID, bridge.ExitNodeID} {
			if _, ok := known[end]; !ok {
				return manifestError(ErrManifestUnknownNode, "связь %d/%s ссылается на ноду %s",
					fleet.FleetID, bridge.RoutingKey, end)
			}
			if _, ok := members[end]; !ok {
				return manifestError(ErrManifestBridgeInvalid, "связь %d/%s: нода %s не входит в fleet",
					fleet.FleetID, bridge.RoutingKey, end)
			}
		}

		pair := pairKey{entry: bridge.EntryNodeID, exit: bridge.ExitNodeID}
		if other, duplicate := seenPairs[pair]; duplicate {
			return manifestError(ErrManifestDuplicate, "fleet %d: связи %s и %s имеют одну пару %s -> %s",
				fleet.FleetID, other, bridge.RoutingKey, bridge.EntryNodeID, bridge.ExitNodeID)
		}
		seenPairs[pair] = bridge.RoutingKey
	}

	return nil
}
