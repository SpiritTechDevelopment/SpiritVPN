package postgres

import (
	"fmt"

	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	"github.com/RomanRyabinkin/SpiritVPN/internal/postgres/db"
)

// Конверсия строк sqlc в доменные типы.
//
// Enum-подобные колонки (kind, desired_state) переносятся приведением типа без
// проверки набора значений: он закреплён CHECK-ограничениями схемы, и
// дублировать их здесь значило бы держать один инвариант в двух местах. Проверяется
// только то, что схема гарантировать не может, — диапазон numeric(20,0).

// entitlementFromRow переносит корневую строку customer в домен.
func entitlementFromRow(row db.CustomerEntitlement) (domain.Entitlement, error) {
	lastCommandNumber, err := uint64FromNumeric(row.LastCommandNumber)
	if err != nil {
		return domain.Entitlement{}, fmt.Errorf("customer_entitlements.last_command_number: %w", err)
	}

	return domain.Entitlement{
		FleetID:           row.VpnFleetID,
		ExpiresAt:         row.ExpiresAt,
		LastCommandNumber: lastCommandNumber,
		DesiredVersion:    row.DesiredVersion,
	}, nil
}

// quotaPeriodFromRow переносит открытый период квоты в домен.
func quotaPeriodFromRow(row db.QuotaPeriod) (domain.QuotaPeriod, error) {
	quotaBytes, err := uint64FromNumeric(row.UsageQuotaBytes)
	if err != nil {
		return domain.QuotaPeriod{}, fmt.Errorf("quota_periods.usage_quota_bytes: %w", err)
	}

	return domain.QuotaPeriod{
		ID:              row.QuotaPeriodID,
		UsageQuotaBytes: quotaBytes,
		StartedAt:       row.StartedAt,
	}, nil
}

// nodeUsageFromRows переносит расход по нодам в домен, сохраняя порядок node_id,
// заданный запросом.
func nodeUsageFromRows(rows []db.LockNodeQuotaUsageRow) ([]domain.NodeQuotaUsage, error) {
	usages := make([]domain.NodeQuotaUsage, 0, len(rows))

	for _, row := range rows {
		totalBytes, err := uint64FromNumeric(row.TotalBytes)
		if err != nil {
			return nil, fmt.Errorf("node_quota_usage.total_bytes (node_id=%s): %w", row.NodeID, err)
		}

		usages = append(usages, domain.NodeQuotaUsage{
			NodeID:      domain.NodeID(row.NodeID),
			TotalBytes:  totalBytes,
			ExhaustedAt: row.ExhaustedAt,
		})
	}

	return usages, nil
}

// accessFromRow переносит строку access в домен. Retired выводится из retired_at:
// домену нужен только факт, а не момент.
func accessFromRow(row db.ListCustomerAccessesRow) domain.Access {
	return domain.Access{
		ID:               row.AccessID,
		Kind:             domain.AccessKind(row.Kind),
		LogicalTargetKey: domain.LogicalTargetKey(row.LogicalTargetKey),
		Generation:       row.Generation,
		EntryNodeID:      domain.NodeID(row.EntryNodeID),
		EgressKey:        row.EgressKey,
		AccountingID:     row.AccountingID,
		DesiredState:     domain.DesiredState(row.DesiredState),
		DesiredVersion:   row.DesiredVersion,
		Retired:          row.RetiredAt != nil,
	}
}

// accessesFromRows сохраняет порядок access_id, заданный запросом.
func accessesFromRows(rows []db.ListCustomerAccessesRow) []domain.Access {
	accesses := make([]domain.Access, 0, len(rows))
	for _, row := range rows {
		accesses = append(accesses, accessFromRow(row))
	}
	return accesses
}

// topologyFromRows собирает проекцию топологии fleet из двух списков.
func topologyFromRows(
	fleetID int64,
	nodes []string,
	routes []db.ListCurrentFleetBridgeRoutesRow,
) domain.FleetTopology {
	topology := domain.FleetTopology{
		FleetID: fleetID,
		Nodes:   make([]domain.NodeID, 0, len(nodes)),
		Bridges: make([]domain.BridgeRoute, 0, len(routes)),
	}

	for _, node := range nodes {
		topology.Nodes = append(topology.Nodes, domain.NodeID(node))
	}

	for _, route := range routes {
		topology.Bridges = append(topology.Bridges, domain.BridgeRoute{
			RoutingKey:  route.RoutingKey,
			EntryNodeID: domain.NodeID(route.EntryNodeID),
			ExitNodeID:  domain.NodeID(route.ExitNodeID),
			EgressTag:   route.EgressTag,
		})
	}

	return topology
}

// nodeIDStrings разворачивает доменные node_id обратно в text[] для запросов,
// принимающих массив. Порядок сохраняется: списки плана уже отсортированы по
// node_id под порядок блокировок.
func nodeIDStrings(nodes []domain.NodeID) []string {
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		ids = append(ids, string(node))
	}
	return ids
}
