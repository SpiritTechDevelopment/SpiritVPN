package postgres

import (
	"context"
	"fmt"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/postgres/db"
)

// ListAvailableNodes читает публичный каталог одним SQL statement, поэтому все
// строки принадлежат одному снимку READ COMMITTED без отдельной транзакции.
func (r *Repository) ListAvailableNodes(ctx context.Context) ([]app.AvailableFleet, error) {
	rows, err := db.New(r.pool).ListAvailableNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: чтение доступных нод: %w", err)
	}

	return availableFleetsFromRows(rows), nil
}

func availableFleetsFromRows(rows []db.ListAvailableNodesRow) []app.AvailableFleet {
	fleets := make([]app.AvailableFleet, 0)
	for _, row := range rows {
		if len(fleets) == 0 || fleets[len(fleets)-1].FleetID != row.VpnFleetID {
			fleets = append(fleets, app.AvailableFleet{FleetID: row.VpnFleetID})
		}

		fleet := &fleets[len(fleets)-1]
		fleet.Nodes = append(fleet.Nodes, app.AvailableNode{
			NodeID:      row.NodeID,
			DisplayName: row.DisplayName,
		})
	}
	return fleets
}
