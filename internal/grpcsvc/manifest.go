package grpcsvc

import (
	"context"
	"math"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	manifestv1 "github.com/RomanRyabinkin/SpiritVPN/internal/gen/spiritvpn/manifest/v1"
)

// applyFleetManifest — use case, который обслуживает метод.
type applyFleetManifest interface {
	Execute(ctx context.Context, cmd app.ApplyManifestCommand) (app.ApplyManifestResult, error)
}

// ManifestServer реализует ManifestService.
type ManifestServer struct {
	manifestv1.UnimplementedManifestServiceServer

	apply applyFleetManifest
}

// NewManifestServer собирает транспорт поверх use case.
func NewManifestServer(apply applyFleetManifest) *ManifestServer {
	return &ManifestServer{apply: apply}
}

// ApplyFleetManifest принимает полный versioned snapshot топологии.
//
// Идентичность вызывающего и request_id берутся здесь, а не внутри use case:
// про mTLS и метадату gRPC знает только транспорт, а аудиту нужны оба.
func (s *ManifestServer) ApplyFleetManifest(
	ctx context.Context,
	req *manifestv1.ApplyFleetManifestRequest,
) (*manifestv1.ApplyFleetManifestResponse, error) {
	snapshot, err := manifestSnapshotFrom(req)
	if err != nil {
		return nil, statusFromError(ctx, err)
	}

	result, err := s.apply.Execute(ctx, app.ApplyManifestCommand{
		Snapshot:         snapshot,
		AllowDestructive: req.GetAllowDestructive(),
		Actor:            peerIdentity(ctx),
		RequestID:        RequestIDFromContext(ctx),
	})
	if err != nil {
		return nil, statusFromError(ctx, err)
	}

	applyResult := manifestv1.ManifestApplyResult_MANIFEST_APPLY_RESULT_APPLIED
	if result.Idempotent {
		applyResult = manifestv1.ManifestApplyResult_MANIFEST_APPLY_RESULT_IDEMPOTENT
	}

	return &manifestv1.ApplyFleetManifestResponse{
		AppliedRevision: uint64(result.Revision),
		Result:          applyResult,
	}, nil
}

// manifestSnapshotFrom переводит запрос в доменный снапшот.
//
// Здесь и только здесь revision сужается с uint64 до int64. Колонка
// manifest_revisions.revision объявлена как bigint, и без проверки значение выше
// 2^63-1 доехало бы до адаптера, где превратилось бы в отрицательное и навсегда
// сломало монотонность. Тот же приём, что и с секундной точностью
// expires_at: сужение под тип колонки закрепляется на границе.
//
// Остальные правила манифеста не дублируются — они в domain.ValidateManifest.
func manifestSnapshotFrom(req *manifestv1.ApplyFleetManifestRequest) (domain.ManifestSnapshot, error) {
	if req.GetRevision() > math.MaxInt64 {
		return domain.ManifestSnapshot{}, &domain.ManifestValidationError{
			Rule:   domain.ErrManifestRevisionInvalid,
			Detail: "значение превышает 2^63-1",
		}
	}

	snapshot := domain.ManifestSnapshot{
		SchemaVersion: req.GetSchemaVersion(),
		Revision:      int64(req.GetRevision()),
		Nodes:         make([]domain.ManifestNode, 0, len(req.GetNodes())),
		Fleets:        make([]domain.ManifestFleet, 0, len(req.GetFleets())),
	}

	for _, node := range req.GetNodes() {
		snapshot.Nodes = append(snapshot.Nodes, manifestNodeFrom(node))
	}
	for _, fleet := range req.GetFleets() {
		snapshot.Fleets = append(snapshot.Fleets, manifestFleetFrom(fleet))
	}

	return snapshot, nil
}

// manifestNodeFrom переносит ноду. display_name в манифесте лежит рядом с
// node_id, а хранится и читается вместе с публичными параметрами;
// перекладка происходит здесь.
//
// Порт сужается с uint32 до int без проверки: диапазон 1..65535 проверяет
// domain.ValidateManifest, а значение выше 2^31 не влезло бы в него и было бы
// отвергнуто там же.
func manifestNodeFrom(node *manifestv1.ManifestNode) domain.ManifestNode {
	return domain.ManifestNode{
		NodeID: domain.NodeID(node.GetNodeId()),
		Agent: domain.NodeAgent{
			Endpoint:            node.GetAgent().GetEndpoint(),
			TLSServerName:       node.GetAgent().GetTlsServerName(),
			CertificateIdentity: node.GetAgent().GetCertificateIdentity(),
		},
		Public: domain.NodePublic{
			Address:          node.GetPublic().GetAddress(),
			Port:             int(node.GetPublic().GetPort()),
			RealityPublicKey: node.GetPublic().GetRealityPublicKey(),
			ServerName:       node.GetPublic().GetServerName(),
			ShortID:          node.GetPublic().GetShortId(),
			Fingerprint:      node.GetPublic().GetFingerprint(),
			Transport:        node.GetPublic().GetTransport(),
			Flow:             node.GetPublic().GetFlow(),
			DisplayName:      node.GetDisplayName(),
		},
	}
}

func manifestFleetFrom(fleet *manifestv1.ManifestFleet) domain.ManifestFleet {
	converted := domain.ManifestFleet{
		FleetID: fleet.GetVpnFleetId(),
		NodeIDs: make([]domain.NodeID, 0, len(fleet.GetNodeIds())),
		Bridges: make([]domain.ManifestBridge, 0, len(fleet.GetBridges())),
	}

	for _, nodeID := range fleet.GetNodeIds() {
		converted.NodeIDs = append(converted.NodeIDs, domain.NodeID(nodeID))
	}
	for _, bridge := range fleet.GetBridges() {
		converted.Bridges = append(converted.Bridges, domain.ManifestBridge{
			RoutingKey:  bridge.GetRoutingKey(),
			EntryNodeID: domain.NodeID(bridge.GetEntryNodeId()),
			ExitNodeID:  domain.NodeID(bridge.GetExitNodeId()),
			EgressTag:   bridge.GetEgressTag(),
			DisplayName: bridge.GetDisplayName(),
		})
	}

	return converted
}
