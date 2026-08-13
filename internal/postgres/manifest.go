package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	"github.com/RomanRyabinkin/SpiritVPN/internal/postgres/db"
)

// manifestIngestLockKey — ключ advisory lock, сериализующего приём манифеста.
//
// Пространство ключей advisory locks общее на всю базу, поэтому значение взято
// заведомо неслучайным и не единицей: с базой backend не делит никто, но цена
// уникального значения нулевая, а цена коллизии — необъяснимое взаимное
// блокирование двух несвязанных подсистем.
const manifestIngestLockKey int64 = 0x5350495256504E01

// WithinManifestTx выполняет приём манифеста в одной транзакции (снапшот
// применяется атомарно либо не применяется совсем).
//
// Уровень READ COMMITTED, как и у командного пути: согласованность
// чтения проекции обеспечивает не изоляция, а advisory lock, который берётся
// первым оператором и снимается вместе с транзакцией.
func (r *Repository) WithinManifestTx(ctx context.Context, fn func(app.ManifestTx) error) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("postgres: начать транзакцию манифеста: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	queries := db.New(tx)
	if err := queries.LockManifestIngest(ctx, manifestIngestLockKey); err != nil {
		return fmt.Errorf("postgres: блокировка приёма манифеста: %w", err)
	}

	if err := fn(&manifestTx{queries: queries}); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit манифеста: %w", err)
	}
	return nil
}

// manifestTx реализует шаги приёма манифеста.
type manifestTx struct {
	queries *db.Queries
}

// LoadProjection читает принятое состояние целиком.
func (t *manifestTx) LoadProjection(ctx context.Context) (domain.ManifestProjection, error) {
	var projection domain.ManifestProjection

	// Отсутствие строк означает, что манифест не принимался ни разу: нулевая
	// LastRevision пропускает любую валидную revision: она всегда > 0.
	last, err := t.queries.GetLastManifestRevision(ctx)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return domain.ManifestProjection{}, err
	default:
		projection.LastRevision = last.Revision
		projection.LastDigest = last.Digest
	}

	nodes, err := t.queries.ListCurrentNodeIDs(ctx)
	if err != nil {
		return domain.ManifestProjection{}, err
	}
	projection.CurrentNodes = nodeIDsFromStrings(nodes)

	projection.Fleets, err = t.queries.ListAcceptedFleetIDs(ctx)
	if err != nil {
		return domain.ManifestProjection{}, err
	}

	memberships, err := t.queries.ListCurrentFleetMemberships(ctx)
	if err != nil {
		return domain.ManifestProjection{}, err
	}
	for _, row := range memberships {
		projection.CurrentMemberships = append(projection.CurrentMemberships, domain.FleetNodeKey{
			FleetID: row.VpnFleetID,
			NodeID:  domain.NodeID(row.NodeID),
		})
	}

	bridges, err := t.queries.ListAllBridgeRoutes(ctx)
	if err != nil {
		return domain.ManifestProjection{}, err
	}
	for _, row := range bridges {
		projection.Bridges = append(projection.Bridges, domain.ProjectedBridge{
			BridgeKey: domain.BridgeKey{
				FleetID:    row.VpnFleetID,
				RoutingKey: row.RoutingKey,
			},
			EntryNodeID: domain.NodeID(row.EntryNodeID),
			ExitNodeID:  domain.NodeID(row.ExitNodeID),
			Current:     row.Current,
		})
	}

	return projection, nil
}

// WritePlan проецирует снапшот.
//
// Порядок между таблицами продиктован внешними ключами: журнал revisions первым
// (на него ссылаются все строки проекции), затем ноды и fleets, и только потом
// membership и связи, которые ссылаются на оба.
//
// Порядок Внутри каждой таблицы — сначала пометка ушедших строк, потом апсерт
// текущих. Это не косметика: пара (entry_node_id, exit_node_id) уникальна среди
// связей с current = true, и при обратном порядке перенос route на новый
// routing_key падал бы на уникальном индексе — в момент вставки новой связи
// старая ещё оставалась бы текущей. Множества «ушедшие» и «текущие» не
// пересекаются по построению плана, поэтому пометка первой ничего не отменяет.
func (t *manifestTx) WritePlan(ctx context.Context, plan domain.ManifestPlan) error {
	if err := t.queries.InsertManifestRevision(ctx, db.InsertManifestRevisionParams{
		Revision:         plan.Revision,
		Digest:           plan.Digest,
		CanonicalPayload: plan.Payload,
	}); err != nil {
		return err
	}

	if err := t.writeNodes(ctx, plan); err != nil {
		return err
	}
	if err := t.writeFleets(ctx, plan); err != nil {
		return err
	}
	if err := t.writeBridges(ctx, plan); err != nil {
		return err
	}

	// Джоба ставится в той же транзакции, что и проекция: принятый снапшот без
	// материализации означал бы customer, навсегда оставшихся без новых access.
	return t.queries.InsertMaterializationJob(ctx, plan.Revision)
}

func (t *manifestTx) writeNodes(ctx context.Context, plan domain.ManifestPlan) error {
	if len(plan.RemovedNodes) > 0 {
		if err := t.queries.RetireNodes(ctx, nodeIDStrings(plan.RemovedNodes)); err != nil {
			return err
		}
	}

	for _, node := range plan.Nodes {
		agent, public := nodeConfigJSON(node)

		if err := t.queries.UpsertVpnNode(ctx, db.UpsertVpnNodeParams{
			NodeID:           string(node.NodeID),
			AgentConfig:      agent,
			PublicConfig:     public,
			ManifestRevision: plan.Revision,
		}); err != nil {
			return err
		}
	}

	return nil
}

func (t *manifestTx) writeFleets(ctx context.Context, plan domain.ManifestPlan) error {
	for _, fleetID := range plan.FleetIDs {
		if err := t.queries.UpsertVpnFleet(ctx, db.UpsertVpnFleetParams{
			VpnFleetID:       fleetID,
			ManifestRevision: plan.Revision,
		}); err != nil {
			return err
		}
	}

	if err := t.retireMemberships(ctx, plan); err != nil {
		return err
	}

	for _, membership := range plan.Memberships {
		if err := t.queries.UpsertFleetNode(ctx, db.UpsertFleetNodeParams{
			VpnFleetID:       membership.FleetID,
			NodeID:           string(membership.NodeID),
			ManifestRevision: plan.Revision,
		}); err != nil {
			return err
		}
	}

	return nil
}

func (t *manifestTx) retireMemberships(ctx context.Context, plan domain.ManifestPlan) error {
	if len(plan.RemovedMemberships) == 0 {
		return nil
	}

	fleetIDs := make([]int64, 0, len(plan.RemovedMemberships))
	nodeIDs := make([]string, 0, len(plan.RemovedMemberships))
	for _, membership := range plan.RemovedMemberships {
		fleetIDs = append(fleetIDs, membership.FleetID)
		nodeIDs = append(nodeIDs, string(membership.NodeID))
	}

	return t.queries.RetireFleetNodes(ctx, db.RetireFleetNodesParams{
		FleetIds: fleetIDs,
		NodeIds:  nodeIDs,
	})
}

func (t *manifestTx) writeBridges(ctx context.Context, plan domain.ManifestPlan) error {
	if err := t.retireBridges(ctx, plan); err != nil {
		return err
	}

	for _, bridge := range plan.Bridges {
		if err := t.queries.UpsertBridgeRoute(ctx, db.UpsertBridgeRouteParams{
			VpnFleetID:       bridge.FleetID,
			RoutingKey:       bridge.RoutingKey,
			EntryNodeID:      string(bridge.EntryNodeID),
			ExitNodeID:       string(bridge.ExitNodeID),
			EgressTag:        bridge.EgressTag,
			DisplayName:      bridge.DisplayName,
			ManifestRevision: plan.Revision,
		}); err != nil {
			return err
		}
	}

	return nil
}

func (t *manifestTx) retireBridges(ctx context.Context, plan domain.ManifestPlan) error {
	if len(plan.RemovedBridges) == 0 {
		return nil
	}

	fleetIDs := make([]int64, 0, len(plan.RemovedBridges))
	routingKeys := make([]string, 0, len(plan.RemovedBridges))
	for _, bridge := range plan.RemovedBridges {
		fleetIDs = append(fleetIDs, bridge.FleetID)
		routingKeys = append(routingKeys, bridge.RoutingKey)
	}

	return t.queries.RetireBridgeRoutes(ctx, db.RetireBridgeRoutesParams{
		FleetIds:    fleetIDs,
		RoutingKeys: routingKeys,
	})
}

// AppendAudit добавляет запись в append-only журнал.
func (t *manifestTx) AppendAudit(ctx context.Context, event app.AuditEvent) error {
	return appendAudit(ctx, t.queries, event)
}

// appendAudit — общая запись журнала для всех путей, которые его ведут (
// customer Apply/renewal/expiry, destructive manifest, смена ключа). Отображение
// AuditEvent в колонки живёт в одном месте, иначе оно разъехалось бы между ними.
func appendAudit(ctx context.Context, queries *db.Queries, event app.AuditEvent) error {
	metadata, err := json.Marshal(event.Metadata)
	if err != nil {
		return fmt.Errorf("сериализация metadata аудита: %w", err)
	}

	return queries.InsertAuditEvent(ctx, db.InsertAuditEventParams{
		ActorType:         event.ActorType,
		ActorID:           optionalText(event.ActorID),
		Action:            event.Action,
		TargetType:        optionalText(event.TargetType),
		TargetID:          optionalText(event.TargetID),
		RequestID:         optionalText(event.RequestID),
		Outcome:           event.Outcome,
		SanitizedMetadata: metadata,
	})
}

// optionalText отображает пустую строку в NULL: колонки аудита nullable, и
// пустая строка в них означала бы «значение есть и оно пустое».
func optionalText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// nodeIDsFromStrings — обратная к nodeIDStrings.
func nodeIDsFromStrings(ids []string) []domain.NodeID {
	nodes := make([]domain.NodeID, 0, len(ids))
	for _, id := range ids {
		nodes = append(nodes, domain.NodeID(id))
	}
	return nodes
}
