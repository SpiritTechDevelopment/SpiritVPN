package domain

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// Фикстуры материализации переиспользуют пример из соседних тестов:
// exampleTopology (две ноды и связь) и materializedAccesses (три согласованных с
// ней access). Customer действует, квота не исчерпана; каждый тест ломает ровно
// одно условие.

func materializeInput() MaterializationInput {
	return MaterializationInput{
		Now:         tNow,
		Entitlement: Entitlement{FleetID: 10, ExpiresAt: tFuture, DesiredVersion: 5},
		Topology:    exampleTopology(),
		Accesses:    materializedAccesses(DesiredStatePresent, 1),
		OpenPeriod:  &QuotaPeriod{ID: accessID(9), UsageQuotaBytes: 1000},
		NodeUsage: []NodeQuotaUsage{
			{NodeID: "NL-1", TotalBytes: 10},
			{NodeID: "DE-1", TotalBytes: 10},
		},
		LiveNodes: []NodeID{"NL-1", "DE-1"},
	}
}

func planOrFailMaterialize(t *testing.T, in MaterializationInput) MaterializationPlan {
	t.Helper()

	plan, err := PlanMaterialize(in)
	if err != nil {
		t.Fatalf("PlanMaterialize: %v", err)
	}
	return plan
}

// TestPlanMaterializeIsIdempotent — повторная материализация одной revision
// идемпотентна. Согласованное состояние обязано давать пустой план.
func TestPlanMaterializeIsIdempotent(t *testing.T) {
	plan := planOrFailMaterialize(t, materializeInput())

	if !plan.IsNoOp() {
		t.Fatalf("согласованное состояние дало непустой план: %+v", plan)
	}
	// Пустой план не двигает версию desired state customer.
	if plan.EntitlementDesiredVersion != 5 {
		t.Errorf("desired_version %d, ожидалась 5", plan.EntitlementDesiredVersion)
	}
}

// TestPlanMaterializeAddsNode — новая нода даёт FREEDOM всем неистёкшим
// customer fleet и нулевую строку расхода текущего периода.
func TestPlanMaterializeAddsNode(t *testing.T) {
	in := materializeInput()
	in.Topology.Nodes = append(in.Topology.Nodes, "FR-1")
	in.LiveNodes = append(in.LiveNodes, "FR-1")

	plan := planOrFailMaterialize(t, in)

	if len(plan.CreateAccesses) != 1 {
		t.Fatalf("создаётся access %d, ожидался 1: %+v", len(plan.CreateAccesses), plan.CreateAccesses)
	}
	created := plan.CreateAccesses[0]
	if created.Kind != AccessKindFreedom || created.LogicalTargetKey != "FR-1" {
		t.Errorf("создан %+v", created)
	}
	if created.DesiredState != DesiredStatePresent || created.DesiredVersion != 1 {
		t.Errorf("состояние нового access %+v", created)
	}
	if created.Generation != 1 {
		t.Errorf("поколение %d, ожидалось 1", created.Generation)
	}

	if len(plan.NodeQuotaInits) != 1 || plan.NodeQuotaInits[0] != "FR-1" {
		t.Errorf("строки расхода %v, ожидалась FR-1", plan.NodeQuotaInits)
	}
	if len(plan.TouchedNodes) != 1 || plan.TouchedNodes[0] != "FR-1" {
		t.Errorf("затронутые ноды %v, ожидалась FR-1", plan.TouchedNodes)
	}
	if plan.EntitlementDesiredVersion != 6 {
		t.Errorf("desired_version %d, ожидалась 6", plan.EntitlementDesiredVersion)
	}
}

// TestPlanMaterializeAddsRelation — новая связь даёт BRIDGE. Строка расхода
// при этом не заводится: нода уже во fleet и свою строку имеет.
func TestPlanMaterializeAddsRelation(t *testing.T) {
	in := materializeInput()
	in.Topology.Bridges = append(in.Topology.Bridges, BridgeRoute{
		RoutingKey:  "de-1.to-nl-1",
		EntryNodeID: "DE-1",
		ExitNodeID:  "NL-1",
		EgressTag:   "nl-exit",
	})

	plan := planOrFailMaterialize(t, in)

	if len(plan.CreateAccesses) != 1 {
		t.Fatalf("создаётся access %d, ожидался 1", len(plan.CreateAccesses))
	}
	created := plan.CreateAccesses[0]
	if created.Kind != AccessKindBridge || created.EntryNodeID != "DE-1" {
		t.Errorf("создан %+v", created)
	}
	if created.EgressKey != "nl-exit" {
		t.Errorf("egress_key %q, ожидался nl-exit", created.EgressKey)
	}
	if len(plan.NodeQuotaInits) != 0 {
		t.Errorf("заведены лишние строки расхода: %v", plan.NodeQuotaInits)
	}
}

// TestPlanMaterializeRetiresWithOperation — удаление связи при живой входной
// ноде создаёт обычный Remove на неё.
func TestPlanMaterializeRetiresWithOperation(t *testing.T) {
	in := materializeInput()
	in.Topology.Bridges = nil

	plan := planOrFailMaterialize(t, in)

	if len(plan.Retire) != 1 {
		t.Fatalf("ретайрится %d access, ожидался 1", len(plan.Retire))
	}
	spec := plan.Retire[0]
	if spec.Access.ID != accessID(3) {
		t.Errorf("ретайрится %v, ожидался BRIDGE", spec.Access.ID)
	}
	if !spec.IssueOperation {
		t.Error("операция не выпущена, хотя входная нода жива")
	}
	if spec.DesiredVersion != 2 {
		t.Errorf("desired_version %d, ожидалась 2", spec.DesiredVersion)
	}
	if len(plan.TouchedNodes) != 1 || plan.TouchedNodes[0] != "NL-1" {
		t.Errorf("затронутые ноды %v, ожидалась NL-1", plan.TouchedNodes)
	}
}

// TestPlanMaterializeRetiresDeadNodeWithoutOperation — глобально удалённая
// нода не получает ни одной команды, а её desired_revision не двигается —
// доставлять туда нечего.
func TestPlanMaterializeRetiresDeadNodeWithoutOperation(t *testing.T) {
	in := materializeInput()
	// DE-1 исчезла из манифеста глобально: ушла и из fleet, и из LiveNodes.
	in.Topology.Nodes = []NodeID{"NL-1"}
	in.Topology.Bridges = nil
	in.LiveNodes = []NodeID{"NL-1"}

	plan := planOrFailMaterialize(t, in)

	if len(plan.Retire) != 2 {
		t.Fatalf("ретайрится %d access, ожидалось 2 (FREEDOM DE-1 и BRIDGE)", len(plan.Retire))
	}

	byID := make(map[uuid.UUID]RetireSpec, len(plan.Retire))
	for _, spec := range plan.Retire {
		byID[spec.Access.ID] = spec
	}

	if byID[accessID(2)].IssueOperation {
		t.Error("выпущена операция на глобально удалённую ноду DE-1")
	}
	// BRIDGE стоял на живой NL-1: его удаление доставляется обычным образом.
	if !byID[accessID(3)].IssueOperation {
		t.Error("операция не выпущена для BRIDGE на живой входной ноде")
	}

	if len(plan.TouchedNodes) != 1 || plan.TouchedNodes[0] != "NL-1" {
		t.Errorf("затронутые ноды %v: удалённая нода не должна попадать в список", plan.TouchedNodes)
	}
}

// TestPlanMaterializeSkipsOperationForAbsentAccess — ретайр уже отсутствующего
// access операции не порождает: она была бы no-op и повисла бы недоставленной в
// метрике.
func TestPlanMaterializeSkipsOperationForAbsentAccess(t *testing.T) {
	in := materializeInput()
	in.Topology.Bridges = nil
	in.Accesses[2].DesiredState = DesiredStateAbsent

	plan := planOrFailMaterialize(t, in)

	if len(plan.Retire) != 1 {
		t.Fatalf("ретайрится %d access, ожидался 1", len(plan.Retire))
	}
	if plan.Retire[0].IssueOperation {
		t.Error("выпущена операция для уже отсутствующего access")
	}
	if len(plan.TouchedNodes) != 0 {
		t.Errorf("затронутые ноды %v, ожидался пустой список", plan.TouchedNodes)
	}
}

// TestPlanMaterializeRepoint — смена egress_tag меняет цель выхода, но не
// поколение и не credentials.
func TestPlanMaterializeRepoint(t *testing.T) {
	in := materializeInput()
	in.Topology.Bridges[0].EgressTag = "de-exit-v2"

	plan := planOrFailMaterialize(t, in)

	if len(plan.Repoints) != 1 {
		t.Fatalf("repoint %d, ожидался 1", len(plan.Repoints))
	}
	change := plan.Repoints[0]
	if change.NewEgressKey != "de-exit-v2" {
		t.Errorf("egress_key %q, ожидался de-exit-v2", change.NewEgressKey)
	}
	if change.DesiredState != DesiredStatePresent {
		t.Errorf("desired_state %s, ожидался PRESENT", change.DesiredState)
	}
	if change.DesiredVersion != 2 {
		t.Errorf("desired_version %d, ожидалась 2: egress_key входит в desired-кортеж", change.DesiredVersion)
	}
	if len(plan.CreateAccesses) != 0 || len(plan.Retire) != 0 {
		t.Error("repoint не должен создавать или ретайрить access")
	}
	if len(plan.TouchedNodes) != 1 || plan.TouchedNodes[0] != "NL-1" {
		t.Errorf("затронутые ноды %v", plan.TouchedNodes)
	}
}

// TestPlanMaterializeExhaustedNodeBornAbsent — новый access на уже исчерпанной
// ноде рождается ABSENT: манифест не должен открывать доступ в обход квоты.
func TestPlanMaterializeExhaustedNodeBornAbsent(t *testing.T) {
	in := materializeInput()
	in.Topology.Bridges = append(in.Topology.Bridges, BridgeRoute{
		RoutingKey:  "de-1.to-nl-1",
		EntryNodeID: "DE-1",
		ExitNodeID:  "NL-1",
		EgressTag:   "nl-exit",
	})
	in.NodeUsage[1].TotalBytes = 1000 // DE-1 исчерпала лимит

	plan := planOrFailMaterialize(t, in)

	if len(plan.CreateAccesses) != 1 {
		t.Fatalf("создаётся access %d, ожидался 1", len(plan.CreateAccesses))
	}
	if plan.CreateAccesses[0].DesiredState != DesiredStateAbsent {
		t.Errorf("desired_state %s, ожидался ABSENT", plan.CreateAccesses[0].DesiredState)
	}

	// Существующий FREEDOM на той же ноде гасится тем же исчерпанием: трафик всех
	// access customer на ноде суммируется в один период.
	if len(plan.DesiredChanges) != 1 || plan.DesiredChanges[0].AccessID != accessID(2) {
		t.Fatalf("смены desired state %+v, ожидалась одна у FREEDOM DE-1", plan.DesiredChanges)
	}
	if plan.DesiredChanges[0].DesiredState != DesiredStateAbsent {
		t.Errorf("FREEDOM DE-1 остался %s", plan.DesiredChanges[0].DesiredState)
	}

	// DE-1 затронута гашением существующего access, а не созданием нового:
	// родившийся ABSENT состава юзеров ноды не меняет.
	if len(plan.TouchedNodes) != 1 || plan.TouchedNodes[0] != "DE-1" {
		t.Errorf("затронутые ноды %v, ожидалась DE-1", plan.TouchedNodes)
	}
}

// TestPlanMaterializeExpiredCustomer — создание ограничено неистёкшими
// customer, но ретайр касается всех: исчезнувшая цель обязана уйти независимо от
// срока.
func TestPlanMaterializeExpiredCustomer(t *testing.T) {
	in := materializeInput()
	in.Entitlement.ExpiresAt = tPast
	in.Topology.Nodes = append(in.Topology.Nodes, "FR-1")
	in.LiveNodes = append(in.LiveNodes, "FR-1")
	in.Topology.Bridges = nil

	plan := planOrFailMaterialize(t, in)

	if len(plan.CreateAccesses) != 0 {
		t.Errorf("истёкшему customer созданы access: %+v", plan.CreateAccesses)
	}
	if len(plan.NodeQuotaInits) != 0 {
		t.Errorf("истёкшему customer заведены строки расхода: %v", plan.NodeQuotaInits)
	}
	if len(plan.Retire) != 1 {
		t.Fatalf("ретайрится %d access, ожидался 1: срок не отменяет ретайр", len(plan.Retire))
	}
}

// TestPlanMaterializeRequiresOpenPeriod — у существующего customer открытый
// период обязан быть. Его отсутствие — нарушение инварианта, а не штатный
// случай.
func TestPlanMaterializeRequiresOpenPeriod(t *testing.T) {
	in := materializeInput()
	in.OpenPeriod = nil

	if _, err := PlanMaterialize(in); !errors.Is(err, ErrOpenPeriodMissing) {
		t.Fatalf("ошибка %v, ожидалась ErrOpenPeriodMissing", err)
	}
}
