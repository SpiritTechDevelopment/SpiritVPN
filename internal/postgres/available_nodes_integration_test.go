package postgres

import (
	"context"
	"testing"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
)

// TestIntegrationListAvailableNodesUsesCurrentManifest проверяет именно SQL-
// семантику каталога: grouping по fleet, одна нода в нескольких fleets и фильтры
// current для ноды, membership и fleet.
func TestIntegrationListAvailableNodesUsesCurrentManifest(t *testing.T) {
	manifestUC, pool := newManifestFixture(t)
	snapshot := manifestFixture(7)
	snapshot.Fleets = append(snapshot.Fleets, domain.ManifestFleet{
		FleetID: testFleetID + 1,
		NodeIDs: []domain.NodeID{"NL-1"},
	})
	applyManifest(t, manifestUC, snapshot, false)

	uc := app.NewListAvailableNodes(New(pool))
	got, err := uc.Execute(context.Background())
	if err != nil {
		t.Fatalf("ListAvailableNodes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("fleets=%d, ожидалось 2: %+v", len(got), got)
	}
	if got[0].FleetID != testFleetID || len(got[0].Nodes) != 2 ||
		got[0].Nodes[0].NodeID != "DE-1" || got[0].Nodes[1].NodeID != "NL-1" {
		t.Fatalf("первый fleet: %+v", got[0])
	}
	if got[1].FleetID != testFleetID+1 || len(got[1].Nodes) != 1 ||
		got[1].Nodes[0].NodeID != "NL-1" {
		t.Fatalf("второй fleet: %+v", got[1])
	}
	if got[0].Nodes[0].DisplayName != "имя DE-1" || got[0].Nodes[1].DisplayName != "имя NL-1" {
		t.Fatalf("display_name не взят из manifest: %+v", got[0].Nodes)
	}

	// Глобально retired NL-1 исчезает сразу из обоих fleets.
	exec(t, pool, `UPDATE vpn_nodes SET current = false WHERE node_id = 'NL-1'`)
	got, err = uc.Execute(context.Background())
	if err != nil {
		t.Fatalf("после retire ноды: %v", err)
	}
	if len(got) != 1 || len(got[0].Nodes) != 1 || got[0].Nodes[0].NodeID != "DE-1" {
		t.Fatalf("после retire ноды: %+v", got)
	}

	// Retired fleet скрывает все его membership.
	exec(t, pool, `UPDATE vpn_fleets SET current = false WHERE vpn_fleet_id = $1`, int64(testFleetID))
	got, err = uc.Execute(context.Background())
	if err != nil {
		t.Fatalf("после retire fleet: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("после retire fleet: %#v, ожидался пустой список", got)
	}
	exec(t, pool, `UPDATE vpn_fleets SET current = true WHERE vpn_fleet_id = $1`, int64(testFleetID))

	// Retired membership скрывает последнюю ноду; пустой fleet не возвращается.
	exec(t, pool, `UPDATE vpn_fleet_nodes SET current = false
	               WHERE vpn_fleet_id = $1 AND node_id = 'DE-1'`, int64(testFleetID))
	got, err = uc.Execute(context.Background())
	if err != nil {
		t.Fatalf("после retire membership: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("после retire membership: %#v, ожидался пустой список", got)
	}
}
