package domain

import (
	"cmp"
	"slices"
)

// FleetNodeKey — членство ноды во fleet (строка vpn_fleet_nodes).
type FleetNodeKey struct {
	FleetID int64
	NodeID  NodeID
}

// BridgeKey — идентичность связи внутри fleet (строка vpn_bridge_routes).
type BridgeKey struct {
	FleetID    int64
	RoutingKey string
}

// ProjectedBridge — связь, уже лежащая в проекции.
type ProjectedBridge struct {
	BridgeKey
	EntryNodeID NodeID
	ExitNodeID  NodeID
	// Current отличает связь текущего снапшота от удалённой. Удалённые нужны
	// потому, что routing_key остаётся занятым навсегда: строка не удаляется, и
	// повторное появление того же ключа обязано описывать ту же пару.
	Current bool
}

// PlannedBridge — связь снапшота, разложенная под строку проекции.
type PlannedBridge struct {
	FleetID     int64
	RoutingKey  string
	EntryNodeID NodeID
	ExitNodeID  NodeID
	EgressTag   string
	DisplayName string
}

// ManifestProjection — принятое состояние, с которым сравнивается снапшот.
type ManifestProjection struct {
	// LastRevision — последняя принятая revision; 0 означает, что манифест не
	// принимался ни разу.
	LastRevision int64
	LastDigest   string

	// CurrentNodes, CurrentMemberships — только строки текущего снапшота: из них
	// вычисляется, что исчезло.
	CurrentNodes       []NodeID
	CurrentMemberships []FleetNodeKey

	// Fleets — ВСЕ принятые fleet. Разделения на текущие и нет здесь нет
	// намеренно: принятый fleet не удаляется и не переиспользуется, а
	// его отсутствие в снапшоте отклоняет весь манифест.
	Fleets []int64

	// Bridges — ВСЕ связи, включая удалённые (см. ProjectedBridge.Current).
	Bridges []ProjectedBridge
}

// ManifestInput — вход планировщика приёма манифеста.
//
// Digest сюда не передаётся: его считает сам PlanManifest. Принимать его
// параметром значило бы допустить вызов, где digest не соответствует снапшоту, —
// а именно на равенстве digest держится вся идемпотентность приёма.
type ManifestInput struct {
	Snapshot ManifestSnapshot
	// AllowDestructive действует только на этот вызов и не сохраняется как
	// разрешение для следующих revisions.
	AllowDestructive bool
	Projection       ManifestProjection
}

// ManifestPlan — что записать, чтобы снапшот стал текущим.
//
// Списки Nodes/FleetIDs/Memberships/Bridges — строки, которые становятся
// текущими; Removed* — строки, которые перестают ими быть. Физических удалений в
// плане нет вовсе: история не удаляется.
type ManifestPlan struct {
	Revision int64
	Digest   string
	// Payload — канонический снапшот; ложится в manifest_revisions как есть,
	// чтобы сохранённый digest был перепроверяем.
	Payload []byte

	// Idempotent — та же revision с тем же digest. План пуст, писать нечего
	// вовсе, включая строку materialization job.
	Idempotent bool

	// Destructive — снапшот что-то удаляет. Гейт по AllowDestructive уже пройден;
	// флаг остаётся для аудита.
	Destructive bool

	Nodes       []ManifestNode
	FleetIDs    []int64
	Memberships []FleetNodeKey
	Bridges     []PlannedBridge

	RemovedNodes       []NodeID
	RemovedMemberships []FleetNodeKey
	RemovedBridges     []BridgeKey
}

// PlanManifest сверяет снапшот с принятым состоянием и раскладывает его в план
// записи.
//
// Снапшот обязан быть уже проверен ValidateManifest: здесь только те правила,
// которым нужно принятое состояние.
//
// Порядок проверок нормативен:
//
//  1. revision и digest — раньше всего. Повтор доставки не должен получать
//     содержательный отказ, а устаревшая revision не должна разбираться по
//     существу;
//  2. присутствие всех ранее принятых fleet — отклоняет манифест независимо от
//     allow_destructive;
//  3. неизменяемость пары у существующего routing_key;
//  4. диф и гейт destructive — последним: он про то, что снапшот удаляет, а это
//     известно только после дифа.
func PlanManifest(in ManifestInput) (ManifestPlan, error) {
	payload, digest := CanonicalizeManifest(in.Snapshot)

	idempotent, err := checkRevision(in, digest)
	if err != nil {
		return ManifestPlan{}, err
	}
	if idempotent {
		return ManifestPlan{Revision: in.Snapshot.Revision, Idempotent: true}, nil
	}

	if err := checkAcceptedFleets(in); err != nil {
		return ManifestPlan{}, err
	}
	if err := checkBridgePairs(in); err != nil {
		return ManifestPlan{}, err
	}

	plan := ManifestPlan{
		Revision: in.Snapshot.Revision,
		Digest:   digest,
		Payload:  payload,
		Nodes:    sortedNodesByID(in.Snapshot.Nodes),
	}
	plan.FleetIDs, plan.Memberships, plan.Bridges = flattenFleets(in.Snapshot.Fleets)

	plan.RemovedNodes = removedNodes(in)
	plan.RemovedMemberships = removedMemberships(in, plan.Memberships)
	plan.RemovedBridges = removedBridges(in, plan.Bridges)

	plan.Destructive = len(plan.RemovedNodes) > 0 ||
		len(plan.RemovedMemberships) > 0 ||
		len(plan.RemovedBridges) > 0

	if plan.Destructive && !in.AllowDestructive {
		return ManifestPlan{}, manifestError(ErrManifestDestructive,
			"нод %d, membership %d, связей %d",
			len(plan.RemovedNodes), len(plan.RemovedMemberships), len(plan.RemovedBridges))
	}

	return plan, nil
}

// checkRevision реализует гейт revision: повтор той же revision с тем же digest
// идемпотентен, с другим digest отклоняется, более старая revision отклоняется.
//
// Идемпотентность распространяется только на ПОСЛЕДНЮЮ принятую revision. Повтор
// более раннего номера — устаревшая доставка, и он отклоняется даже при
// совпадающем digest: более специальное правило про старую revision
// перекрывает общее про повтор.
func checkRevision(in ManifestInput, digest string) (idempotent bool, err error) {
	switch {
	case in.Snapshot.Revision == in.Projection.LastRevision:
		if digest == in.Projection.LastDigest {
			return true, nil
		}
		return false, manifestError(ErrManifestDigestConflict, "revision %d", in.Snapshot.Revision)

	case in.Snapshot.Revision < in.Projection.LastRevision:
		return false, manifestError(ErrManifestRevisionRegression, "получена %d, принята %d",
			in.Snapshot.Revision, in.Projection.LastRevision)

	default:
		return false, nil
	}
}

// checkAcceptedFleets — все fleet ID из предыдущего принятого снапшота
// обязаны присутствовать; отсутствие хотя бы одного отклоняет весь манифест
// независимо от allow_destructive.
func checkAcceptedFleets(in ManifestInput) error {
	present := make(map[int64]struct{}, len(in.Snapshot.Fleets))
	for _, fleet := range in.Snapshot.Fleets {
		present[fleet.FleetID] = struct{}{}
	}

	missing := make([]int64, 0)
	for _, accepted := range in.Projection.Fleets {
		if _, ok := present[accepted]; !ok {
			missing = append(missing, accepted)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	slices.Sort(missing)
	return manifestError(ErrManifestFleetMissing, "%v", missing)
}

// checkBridgePairs — пара (entry, exit) неизменяема для существующего
// routing_key.
//
// Сверка идёт со ВСЕМИ строками, включая удалённые. Строка живёт вечно, и
// оживление её с другой парой означало бы, что одна и та же логическая цель
// (logical_target_key = routing_key) вдруг указывает в другое место, унося за
// собой входную ноду всех уже выданных access. Перенос требует именно
// новый routing_key.
func checkBridgePairs(in ManifestInput) error {
	projected := make(map[BridgeKey]ProjectedBridge, len(in.Projection.Bridges))
	for _, bridge := range in.Projection.Bridges {
		projected[bridge.BridgeKey] = bridge
	}

	for _, fleet := range in.Snapshot.Fleets {
		for _, bridge := range fleet.Bridges {
			key := BridgeKey{FleetID: fleet.FleetID, RoutingKey: bridge.RoutingKey}

			existing, known := projected[key]
			if !known {
				continue
			}
			if existing.EntryNodeID != bridge.EntryNodeID || existing.ExitNodeID != bridge.ExitNodeID {
				return manifestError(ErrManifestBridgePairImmutable,
					"связь %d/%s: принято %s -> %s, получено %s -> %s",
					fleet.FleetID, bridge.RoutingKey,
					existing.EntryNodeID, existing.ExitNodeID,
					bridge.EntryNodeID, bridge.ExitNodeID)
			}
		}
	}

	return nil
}

// sortedNodesByID упорядочивает ноды по node_id: в этом порядке транзакция
// обязана брать их row locks.
func sortedNodesByID(nodes []ManifestNode) []ManifestNode {
	sorted := slices.Clone(nodes)
	slices.SortFunc(sorted, func(a, b ManifestNode) int {
		return cmp.Compare(a.NodeID, b.NodeID)
	})
	return sorted
}

// flattenFleets раскладывает fleets в три плоских отсортированных списка —
// по одному на таблицу проекции.
func flattenFleets(fleets []ManifestFleet) ([]int64, []FleetNodeKey, []PlannedBridge) {
	ids := make([]int64, 0, len(fleets))
	memberships := make([]FleetNodeKey, 0)
	bridges := make([]PlannedBridge, 0)

	for _, fleet := range fleets {
		ids = append(ids, fleet.FleetID)

		for _, nodeID := range fleet.NodeIDs {
			memberships = append(memberships, FleetNodeKey{FleetID: fleet.FleetID, NodeID: nodeID})
		}

		for _, bridge := range fleet.Bridges {
			bridges = append(bridges, PlannedBridge{
				FleetID:     fleet.FleetID,
				RoutingKey:  bridge.RoutingKey,
				EntryNodeID: bridge.EntryNodeID,
				ExitNodeID:  bridge.ExitNodeID,
				EgressTag:   bridge.EgressTag,
				DisplayName: bridge.DisplayName,
			})
		}
	}

	slices.Sort(ids)
	slices.SortFunc(memberships, compareFleetNodeKey)
	slices.SortFunc(bridges, func(a, b PlannedBridge) int {
		return cmp.Or(
			cmp.Compare(a.FleetID, b.FleetID),
			cmp.Compare(a.RoutingKey, b.RoutingKey),
		)
	})

	return ids, memberships, bridges
}

// removedNodes — ноды, глобально исчезнувшие из манифеста. Backend
// прекращает выдавать их ссылки и не доставляет на них операции; ретайр самих
// access принадлежит materialization job.
func removedNodes(in ManifestInput) []NodeID {
	present := make(map[NodeID]struct{}, len(in.Snapshot.Nodes))
	for _, node := range in.Snapshot.Nodes {
		present[node.NodeID] = struct{}{}
	}

	removed := make([]NodeID, 0)
	for _, node := range in.Projection.CurrentNodes {
		if _, ok := present[node]; !ok {
			removed = append(removed, node)
		}
	}

	slices.Sort(removed)
	return removed
}

// removedMemberships — ноды, вышедшие из fleet при сохранении самой ноды.
func removedMemberships(in ManifestInput, present []FleetNodeKey) []FleetNodeKey {
	set := make(map[FleetNodeKey]struct{}, len(present))
	for _, key := range present {
		set[key] = struct{}{}
	}

	removed := make([]FleetNodeKey, 0)
	for _, key := range in.Projection.CurrentMemberships {
		if _, ok := set[key]; !ok {
			removed = append(removed, key)
		}
	}

	slices.SortFunc(removed, compareFleetNodeKey)
	return removed
}

// removedBridges — связи, исчезнувшие из манифеста. Сравниваются только текущие:
// уже удалённая связь второй раз не удаляется и destructive-флага не требует.
func removedBridges(in ManifestInput, present []PlannedBridge) []BridgeKey {
	set := make(map[BridgeKey]struct{}, len(present))
	for _, bridge := range present {
		set[BridgeKey{FleetID: bridge.FleetID, RoutingKey: bridge.RoutingKey}] = struct{}{}
	}

	removed := make([]BridgeKey, 0)
	for _, bridge := range in.Projection.Bridges {
		if !bridge.Current {
			continue
		}
		if _, ok := set[bridge.BridgeKey]; !ok {
			removed = append(removed, bridge.BridgeKey)
		}
	}

	slices.SortFunc(removed, func(a, b BridgeKey) int {
		return cmp.Or(
			cmp.Compare(a.FleetID, b.FleetID),
			cmp.Compare(a.RoutingKey, b.RoutingKey),
		)
	})
	return removed
}

func compareFleetNodeKey(a, b FleetNodeKey) int {
	return cmp.Or(
		cmp.Compare(a.FleetID, b.FleetID),
		cmp.Compare(a.NodeID, b.NodeID),
	)
}
