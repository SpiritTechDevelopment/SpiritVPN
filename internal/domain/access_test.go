package domain

import (
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// accessID даёт детерминированные UUID: порядок байт совпадает с порядком n,
// поэтому сортировка по access_id в тестах предсказуема.
func accessID(n byte) uuid.UUID {
	var id uuid.UUID
	id[15] = n
	return id
}

// exampleTopology — пример из §4: ноды NL-1 и DE-1 плюс связь NL-1 -> DE-1 дают
// customer три access.
func exampleTopology() FleetTopology {
	return FleetTopology{
		FleetID: 10,
		Nodes:   []NodeID{"NL-1", "DE-1"},
		Bridges: []BridgeRoute{{
			RoutingKey:  "nl-1.to-de-1",
			EntryNodeID: "NL-1",
			ExitNodeID:  "DE-1",
			EgressTag:   "de-exit",
		}},
	}
}

// label — компактное представление цели для сравнения планов в тестах.
func label(kind AccessKind, key LogicalTargetKey) string {
	return fmt.Sprintf("%s/%s", kind, key)
}

func createLabels(specs []NewAccessSpec) []string {
	labels := make([]string, 0, len(specs))
	for _, spec := range specs {
		labels = append(labels, label(spec.Kind, spec.LogicalTargetKey))
	}
	return labels
}

func accessLabels(accesses []Access) []string {
	labels := make([]string, 0, len(accesses))
	for _, access := range accesses {
		labels = append(labels, label(access.Kind, access.LogicalTargetKey))
	}
	return labels
}

// §4: link_count = fleet_node_count + bridge_relation_count.
func TestTargetsOf(t *testing.T) {
	targets := TargetsOf(exampleTopology())

	want := []Target{
		{
			Kind:             AccessKindBridge,
			LogicalTargetKey: "nl-1.to-de-1",
			// Для BRIDGE входной нодой является entry_node_id связи; на EXIT
			// customer credential не ставится.
			EntryNodeID: "NL-1",
			EgressKey:   "de-exit",
		},
		{
			Kind:             AccessKindFreedom,
			LogicalTargetKey: "DE-1",
			EntryNodeID:      "DE-1",
			EgressKey:        FreedomEgressKey,
		},
		{
			Kind:             AccessKindFreedom,
			LogicalTargetKey: "NL-1",
			EntryNodeID:      "NL-1",
			EgressKey:        FreedomEgressKey,
		},
	}

	if !slices.Equal(targets, want) {
		t.Fatalf("TargetsOf() = %+v, ожидалось %+v", targets, want)
	}
}

// Несколько BRIDGE с одной входной ноды допустимы: ограничения «не более одного
// BRIDGE на customer/node» нет (§4).
func TestTargetsOfMultipleBridgesFromSameEntry(t *testing.T) {
	topology := FleetTopology{
		FleetID: 10,
		Nodes:   []NodeID{"NL-1", "DE-1", "FR-1"},
		Bridges: []BridgeRoute{
			{RoutingKey: "nl-1.to-de-1", EntryNodeID: "NL-1", ExitNodeID: "DE-1", EgressTag: "de-exit"},
			{RoutingKey: "nl-1.to-fr-1", EntryNodeID: "NL-1", ExitNodeID: "FR-1", EgressTag: "fr-exit"},
		},
	}

	targets := TargetsOf(topology)
	if len(targets) != 5 {
		t.Fatalf("link_count = %d, ожидалось 5 (3 ноды + 2 связи)", len(targets))
	}

	var fromNL int
	for _, target := range targets {
		if target.Kind == AccessKindBridge && target.EntryNodeID == "NL-1" {
			fromNL++
		}
	}
	if fromNL != 2 {
		t.Fatalf("BRIDGE с входом NL-1 = %d, ожидалось 2", fromNL)
	}
}

func TestPlanAccessSetCreatesMissing(t *testing.T) {
	plan := PlanAccessSet(exampleTopology(), nil)

	want := []string{"BRIDGE/nl-1.to-de-1", "FREEDOM/DE-1", "FREEDOM/NL-1"}
	if got := createLabels(plan.Create); !slices.Equal(got, want) {
		t.Fatalf("Create = %v, ожидалось %v", got, want)
	}
	if len(plan.InSync)+len(plan.Retire)+len(plan.Repoint) != 0 {
		t.Fatalf("ожидались только Create, получено %+v", plan)
	}

	for _, spec := range plan.Create {
		if spec.Generation != 1 {
			t.Fatalf("%s: поколение = %d, ожидалось 1", label(spec.Kind, spec.LogicalTargetKey), spec.Generation)
		}
	}

	// egress_key: пустой для FREEDOM (локальный выход), egress_tag для BRIDGE.
	for _, spec := range plan.Create {
		wantEgress := FreedomEgressKey
		if spec.Kind == AccessKindBridge {
			wantEgress = "de-exit"
		}
		if spec.EgressKey != wantEgress {
			t.Fatalf("%s: egress_key = %q, ожидалось %q",
				label(spec.Kind, spec.LogicalTargetKey), spec.EgressKey, wantEgress)
		}
	}
}

func TestPlanAccessSetKeepsInSync(t *testing.T) {
	accesses := []Access{
		{ID: accessID(1), Kind: AccessKindFreedom, LogicalTargetKey: "NL-1", Generation: 1, EntryNodeID: "NL-1"},
		{ID: accessID(2), Kind: AccessKindFreedom, LogicalTargetKey: "DE-1", Generation: 1, EntryNodeID: "DE-1"},
		{ID: accessID(3), Kind: AccessKindBridge, LogicalTargetKey: "nl-1.to-de-1", Generation: 1, EntryNodeID: "NL-1", EgressKey: "de-exit"},
	}

	plan := PlanAccessSet(exampleTopology(), accesses)

	if len(plan.Create) != 0 || len(plan.Retire) != 0 || len(plan.Repoint) != 0 {
		t.Fatalf("ожидался полностью согласованный набор, получено %+v", plan)
	}
	want := []string{"BRIDGE/nl-1.to-de-1", "FREEDOM/DE-1", "FREEDOM/NL-1"}
	if got := accessLabels(plan.InSync); !slices.Equal(got, want) {
		t.Fatalf("InSync = %v, ожидалось %v", got, want)
	}
}

// §6: смена egress_tag при неизменных routing_key и паре (entry, exit) — repoint,
// а не новое поколение.
func TestPlanAccessSetDetectsEgressDrift(t *testing.T) {
	accesses := []Access{
		{ID: accessID(1), Kind: AccessKindFreedom, LogicalTargetKey: "NL-1", Generation: 1, EntryNodeID: "NL-1"},
		{ID: accessID(2), Kind: AccessKindFreedom, LogicalTargetKey: "DE-1", Generation: 1, EntryNodeID: "DE-1"},
		{ID: accessID(3), Kind: AccessKindBridge, LogicalTargetKey: "nl-1.to-de-1", Generation: 1, EntryNodeID: "NL-1", EgressKey: "de-exit-old"},
	}

	plan := PlanAccessSet(exampleTopology(), accesses)

	if len(plan.Repoint) != 1 {
		t.Fatalf("Repoint = %+v, ожидалась одна запись", plan.Repoint)
	}
	if plan.Repoint[0].Access.ID != accessID(3) || plan.Repoint[0].NewEgressKey != "de-exit" {
		t.Fatalf("Repoint[0] = %+v, ожидался access 3 с egress de-exit", plan.Repoint[0])
	}
	// Рассогласованный access не считается согласованным и не порождает поколения.
	if got := accessLabels(plan.InSync); !slices.Equal(got, []string{"FREEDOM/DE-1", "FREEDOM/NL-1"}) {
		t.Fatalf("InSync = %v, ожидались только FREEDOM", got)
	}
	if len(plan.Create) != 0 {
		t.Fatalf("Create = %v, дрейф egress не должен создавать новый access", createLabels(plan.Create))
	}
}

func TestPlanAccessSetRetiresVanishedTarget(t *testing.T) {
	topology := FleetTopology{FleetID: 10, Nodes: []NodeID{"NL-1", "DE-1"}}

	accesses := []Access{
		{ID: accessID(1), Kind: AccessKindFreedom, LogicalTargetKey: "NL-1", Generation: 1, EntryNodeID: "NL-1"},
		{ID: accessID(2), Kind: AccessKindFreedom, LogicalTargetKey: "DE-1", Generation: 1, EntryNodeID: "DE-1"},
		// Связь удалена из manifest, входная нода жива.
		{ID: accessID(3), Kind: AccessKindBridge, LogicalTargetKey: "nl-1.to-de-1", Generation: 1, EntryNodeID: "NL-1", EgressKey: "de-exit"},
	}

	plan := PlanAccessSet(topology, accesses)

	if got := accessLabels(plan.Retire); !slices.Equal(got, []string{"BRIDGE/nl-1.to-de-1"}) {
		t.Fatalf("Retire = %v, ожидался ушедший bridge", got)
	}
	if got := accessLabels(plan.InSync); !slices.Equal(got, []string{"FREEDOM/DE-1", "FREEDOM/NL-1"}) {
		t.Fatalf("InSync = %v, ожидались обе FREEDOM", got)
	}
}

// План не должен зависеть от обхода map: Retire упорядочен по
// (kind, logical_target_key, access_id).
func TestPlanAccessSetRetireIsSorted(t *testing.T) {
	// Fleet опустел целиком — все текущие access остались без цели.
	topology := FleetTopology{FleetID: 10}

	accesses := []Access{
		{ID: accessID(5), Kind: AccessKindFreedom, LogicalTargetKey: "NL-1", Generation: 1, EntryNodeID: "NL-1"},
		{ID: accessID(1), Kind: AccessKindBridge, LogicalTargetKey: "b-2", Generation: 1, EntryNodeID: "NL-1"},
		{ID: accessID(9), Kind: AccessKindBridge, LogicalTargetKey: "b-1", Generation: 1, EntryNodeID: "NL-1"},
		{ID: accessID(3), Kind: AccessKindFreedom, LogicalTargetKey: "DE-1", Generation: 1, EntryNodeID: "DE-1"},
	}

	plan := PlanAccessSet(topology, accesses)

	want := []string{"BRIDGE/b-1", "BRIDGE/b-2", "FREEDOM/DE-1", "FREEDOM/NL-1"}
	if got := accessLabels(plan.Retire); !slices.Equal(got, want) {
		t.Fatalf("Retire = %v, ожидалось %v", got, want)
	}
}

// §4 и §17: fleet может временно не содержать ни нод, ни связей, оставаясь
// действующим. Набор целей тогда пуст.
func TestPlanAccessSetEmptyFleet(t *testing.T) {
	plan := PlanAccessSet(FleetTopology{FleetID: 10}, nil)

	if len(plan.Create)+len(plan.InSync)+len(plan.Retire)+len(plan.Repoint) != 0 {
		t.Fatalf("у пустого fleet не должно быть ни целей, ни расхождений: %+v", plan)
	}
}

// §4: повторно добавленная logical target даёт новое поколение max+1 с новыми
// credentials; ретайрнутые access остаются в истории и не восстанавливаются.
func TestPlanAccessSetNextGeneration(t *testing.T) {
	topology := FleetTopology{FleetID: 10, Nodes: []NodeID{"NL-1"}}

	accesses := []Access{
		{ID: accessID(1), Kind: AccessKindFreedom, LogicalTargetKey: "NL-1", Generation: 1, EntryNodeID: "NL-1", Retired: true},
		{ID: accessID(2), Kind: AccessKindFreedom, LogicalTargetKey: "NL-1", Generation: 2, EntryNodeID: "NL-1", Retired: true},
	}

	plan := PlanAccessSet(topology, accesses)

	if len(plan.Create) != 1 {
		t.Fatalf("Create = %v, ожидалась одна запись", createLabels(plan.Create))
	}
	if plan.Create[0].Generation != 3 {
		t.Fatalf("поколение = %d, ожидалось 3 (max+1)", plan.Create[0].Generation)
	}
	// Ретайрнутые в текущий набор не попадают.
	if len(plan.InSync) != 0 || len(plan.Retire) != 0 {
		t.Fatalf("ретайрнутые access не должны попадать в InSync/Retire: %+v", plan)
	}
}
