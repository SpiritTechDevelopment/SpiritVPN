package postgres

import (
	"testing"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/postgres/db"
)

var _ app.AvailableNodesRepository = (*Repository)(nil)

func TestAvailableFleetsFromRowsGroupsOrderedRows(t *testing.T) {
	rows := []db.ListAvailableNodesRow{
		{VpnFleetID: 10, NodeID: "DE-1", DisplayName: "Германия"},
		{VpnFleetID: 10, NodeID: "NL-1", DisplayName: "Нидерланды"},
		{VpnFleetID: 20, NodeID: "NL-1", DisplayName: "Нидерланды"},
	}

	got := availableFleetsFromRows(rows)
	if len(got) != 2 {
		t.Fatalf("fleets=%d, ожидалось 2", len(got))
	}
	if got[0].FleetID != 10 || len(got[0].Nodes) != 2 || got[0].Nodes[0].NodeID != "DE-1" ||
		got[0].Nodes[1].DisplayName != "Нидерланды" {
		t.Fatalf("первый fleet: %+v", got[0])
	}
	if got[1].FleetID != 20 || len(got[1].Nodes) != 1 || got[1].Nodes[0].NodeID != "NL-1" {
		t.Fatalf("второй fleet: %+v", got[1])
	}
}

func TestAvailableFleetsFromRowsEmpty(t *testing.T) {
	got := availableFleetsFromRows(nil)
	if got == nil || len(got) != 0 {
		t.Fatalf("результат %#v, ожидался непустой пустой slice", got)
	}
}
