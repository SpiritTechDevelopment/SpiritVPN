package domain

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// Три access примера: FREEDOM на каждую ноду и BRIDGE на связь. У обоих
// access с входом NL-1 общая node quota — на этом проверяется, что исчерпание
// гасит их вместе.
func materializedAccesses(state DesiredState, version int64) []Access {
	return []Access{
		{
			ID: accessID(1), Kind: AccessKindFreedom, LogicalTargetKey: "NL-1", Generation: 1,
			EntryNodeID: "NL-1", DesiredState: state, DesiredVersion: version,
		},
		{
			ID: accessID(2), Kind: AccessKindFreedom, LogicalTargetKey: "DE-1", Generation: 1,
			EntryNodeID: "DE-1", DesiredState: state, DesiredVersion: version,
		},
		{
			ID: accessID(3), Kind: AccessKindBridge, LogicalTargetKey: "nl-1.to-de-1", Generation: 1,
			EntryNodeID: "NL-1", EgressKey: "de-exit", DesiredState: state, DesiredVersion: version,
		},
	}
}

func existingCustomer(expiresAt time.Time) *Entitlement {
	return &Entitlement{FleetID: 10, ExpiresAt: expiresAt, LastCommandNumber: 5, DesiredVersion: 3}
}

func changedAccessIDs(changes []DesiredChange) []string {
	ids := make([]string, 0, len(changes))
	for _, change := range changes {
		ids = append(ids, change.AccessID.String())
	}
	return ids
}

func findChange(t *testing.T, changes []DesiredChange, id byte) DesiredChange {
	t.Helper()

	want := accessID(id)
	for _, change := range changes {
		if change.AccessID == want {
			return change
		}
	}
	t.Fatalf("изменение для access %d не найдено, есть только %v", id, changedAccessIDs(changes))
	return DesiredChange{}
}

// Первый Apply: корневая строка, начальный период и полный набор access.
func TestPlanApplyCreatesCustomer(t *testing.T) {
	cmd := validCommand()
	cmd.ExpiresAt = tFuture

	plan, err := PlanApply(ApplyInput{
		Now:      tNow,
		Command:  cmd,
		Topology: exampleTopology(),
	})
	if err != nil {
		t.Fatalf("PlanApply() ошибка: %v", err)
	}

	if plan.Decision != ApplyDecisionCreate {
		t.Fatalf("решение = %v, ожидалось CREATE", plan.Decision)
	}
	if !plan.CreateEntitlement || !plan.OpenNewPeriod {
		t.Fatalf("ожидались создание корневой строки и открытие периода, получено %+v", plan)
	}
	if plan.UpdatePeriodQuota {
		t.Fatal("новый период уже несёт нужную квоту, отдельное обновление не требуется")
	}
	if plan.IsNoOp() {
		t.Fatal("создание customer не может быть no-op")
	}

	// Период открывается с нулевым расходом на каждой ноде fleet.
	if got := plan.NodeQuotaInits; !slices.Equal(got, []NodeID{"DE-1", "NL-1"}) {
		t.Fatalf("NodeQuotaInits = %v, ожидались обе ноды по возрастанию node_id", got)
	}
	if len(plan.NodeQuotaChanges) != 0 {
		t.Fatalf("NodeQuotaChanges = %+v, у нового периода исчерпанных нод нет", plan.NodeQuotaChanges)
	}

	if got := createLabels(plan.CreateAccesses); !slices.Equal(got,
		[]string{"BRIDGE/nl-1.to-de-1", "FREEDOM/DE-1", "FREEDOM/NL-1"}) {
		t.Fatalf("CreateAccesses = %v, ожидались все три цели", got)
	}
	for _, spec := range plan.CreateAccesses {
		if spec.DesiredState != DesiredStatePresent {
			t.Fatalf("%s: desired = %v, ожидалось PRESENT", spec.LogicalTargetKey, spec.DesiredState)
		}
		if spec.DesiredVersion != 1 {
			t.Fatalf("%s: версия = %d, ожидалась 1", spec.LogicalTargetKey, spec.DesiredVersion)
		}
	}

	if got := plan.TouchedNodes; !slices.Equal(got, []NodeID{"DE-1", "NL-1"}) {
		t.Fatalf("TouchedNodes = %v, ожидались обе ноды", got)
	}
	if plan.EntitlementDesiredVersion != 1 {
		t.Fatalf("версия entitlement = %d, ожидалась 1", plan.EntitlementDesiredVersion)
	}
}

// Принятая команда, не меняющая целевого состояния, не создаёт
// ни операций, ни нового периода. last_command_number при этом всё равно
// двигается — но это уже дело слоя приложения.
func TestPlanApplyExactRepeatIsNoOp(t *testing.T) {
	cmd := validCommand()
	cmd.ExpiresAt = tFuture
	cmd.CommandNumber = 6

	entitlement := existingCustomer(tFuture)

	plan, err := PlanApply(ApplyInput{
		Now:         tNow,
		Command:     cmd,
		Entitlement: entitlement,
		Topology:    exampleTopology(),
		Accesses:    materializedAccesses(DesiredStatePresent, 1),
		OpenPeriod:  &QuotaPeriod{ID: accessID(200), UsageQuotaBytes: cmd.UsageQuotaBytes, StartedAt: tPast},
		NodeUsage: []NodeQuotaUsage{
			{NodeID: "NL-1", TotalBytes: 10},
			{NodeID: "DE-1", TotalBytes: 20},
		},
	})
	if err != nil {
		t.Fatalf("PlanApply() ошибка: %v", err)
	}

	if plan.Decision != ApplyDecisionQuotaChange {
		t.Fatalf("решение = %v, ожидалось QUOTA_CHANGE", plan.Decision)
	}
	if !plan.IsNoOp() {
		t.Fatalf("точный повтор обязан быть no-op, получено %+v", plan)
	}
	if plan.EntitlementDesiredVersion != entitlement.DesiredVersion {
		t.Fatalf("версия entitlement = %d, на пустом плане должна остаться %d",
			plan.EntitlementDesiredVersion, entitlement.DesiredVersion)
	}
}

// Renewal истёкшего customer открывает новый период и возвращает
// access в PRESENT.
func TestPlanApplyRenewalOfExpiredCustomer(t *testing.T) {
	cmd := validCommand()
	cmd.ExpiresAt = tLater
	cmd.CommandNumber = 6

	exhausted := tPast

	plan, err := PlanApply(ApplyInput{
		Now:         tNow,
		Command:     cmd,
		Entitlement: existingCustomer(tPast),
		Topology:    exampleTopology(),
		Accesses:    materializedAccesses(DesiredStateAbsent, 2),
		OpenPeriod:  &QuotaPeriod{ID: accessID(200), UsageQuotaBytes: 1 << 20, StartedAt: tPast},
		// Расход старого периода в новый не переносится, отметка исчерпания тоже.
		NodeUsage: []NodeQuotaUsage{
			{NodeID: "NL-1", TotalBytes: 1 << 40, ExhaustedAt: &exhausted},
			{NodeID: "DE-1", TotalBytes: 1 << 40, ExhaustedAt: &exhausted},
		},
	})
	if err != nil {
		t.Fatalf("PlanApply() ошибка: %v", err)
	}

	if plan.Decision != ApplyDecisionRenewal || !plan.OpenNewPeriod {
		t.Fatalf("ожидался renewal с новым периодом, получено %+v", plan)
	}
	if len(plan.NodeQuotaChanges) != 0 {
		t.Fatalf("NodeQuotaChanges = %+v, у нового периода отметок нет по построению", plan.NodeQuotaChanges)
	}
	if got := plan.NodeQuotaInits; !slices.Equal(got, []NodeID{"DE-1", "NL-1"}) {
		t.Fatalf("NodeQuotaInits = %v, ожидались обе ноды fleet", got)
	}

	if len(plan.DesiredChanges) != 3 {
		t.Fatalf("DesiredChanges = %+v, ожидались все три access", plan.DesiredChanges)
	}
	for _, change := range plan.DesiredChanges {
		if change.DesiredState != DesiredStatePresent {
			t.Fatalf("access %s: desired = %v, ожидалось PRESENT", change.AccessID, change.DesiredState)
		}
		if change.DesiredVersion != 3 {
			t.Fatalf("access %s: версия = %d, ожидалась 3 (прежняя 2 + 1)", change.AccessID, change.DesiredVersion)
		}
	}
	if got := plan.TouchedNodes; !slices.Equal(got, []NodeID{"DE-1", "NL-1"}) {
		t.Fatalf("TouchedNodes = %v, ожидались обе ноды", got)
	}
	if plan.EntitlementDesiredVersion != 4 {
		t.Fatalf("версия entitlement = %d, ожидалась 4", plan.EntitlementDesiredVersion)
	}
}

// Квота независима на каждой ноде. Оба access с входом NL-1 гаснут вместе, а
// DE-1 не затрагивается.
func TestPlanApplyQuotaLoweredExhaustsSingleNode(t *testing.T) {
	cmd := validCommand()
	cmd.ExpiresAt = tFuture
	cmd.UsageQuotaBytes = 100
	cmd.CommandNumber = 6

	plan, err := PlanApply(ApplyInput{
		Now:         tNow,
		Command:     cmd,
		Entitlement: existingCustomer(tFuture),
		Topology:    exampleTopology(),
		Accesses:    materializedAccesses(DesiredStatePresent, 1),
		OpenPeriod:  &QuotaPeriod{ID: accessID(200), UsageQuotaBytes: 200, StartedAt: tPast},
		NodeUsage: []NodeQuotaUsage{
			{NodeID: "NL-1", TotalBytes: 150},
			{NodeID: "DE-1", TotalBytes: 10},
		},
	})
	if err != nil {
		t.Fatalf("PlanApply() ошибка: %v", err)
	}

	if !plan.UpdatePeriodQuota || plan.OpenNewPeriod {
		t.Fatalf("ожидалась смена лимита без нового периода, получено %+v", plan)
	}
	assertQuotaChanges(t, plan.NodeQuotaChanges, []NodeQuotaChange{{NodeID: "NL-1", ExhaustedAt: &tNow}})

	// Гаснут ровно два access входной ноды NL-1: FREEDOM и BRIDGE.
	if len(plan.DesiredChanges) != 2 {
		t.Fatalf("DesiredChanges = %v, ожидались оба access ноды NL-1", changedAccessIDs(plan.DesiredChanges))
	}
	for _, id := range []byte{1, 3} {
		change := findChange(t, plan.DesiredChanges, id)
		if change.DesiredState != DesiredStateAbsent {
			t.Fatalf("access %d: desired = %v, ожидалось ABSENT", id, change.DesiredState)
		}
		if change.DesiredVersion != 2 {
			t.Fatalf("access %d: версия = %d, ожидалась 2", id, change.DesiredVersion)
		}
	}
	if got := plan.TouchedNodes; !slices.Equal(got, []NodeID{"NL-1"}) {
		t.Fatalf("TouchedNodes = %v, ожидалась только NL-1", got)
	}
}

// Повышение лимита снимает отметку и разблокирует ноду независимо от других.
func TestPlanApplyQuotaRaisedUnblocksNode(t *testing.T) {
	cmd := validCommand()
	cmd.ExpiresAt = tFuture
	cmd.UsageQuotaBytes = 500
	cmd.CommandNumber = 6

	exhausted := tPast

	accesses := materializedAccesses(DesiredStatePresent, 1)
	accesses[0].DesiredState = DesiredStateAbsent // FREEDOM NL-1
	accesses[2].DesiredState = DesiredStateAbsent // BRIDGE с входом NL-1

	plan, err := PlanApply(ApplyInput{
		Now:         tNow,
		Command:     cmd,
		Entitlement: existingCustomer(tFuture),
		Topology:    exampleTopology(),
		Accesses:    accesses,
		OpenPeriod:  &QuotaPeriod{ID: accessID(200), UsageQuotaBytes: 100, StartedAt: tPast},
		NodeUsage: []NodeQuotaUsage{
			{NodeID: "NL-1", TotalBytes: 150, ExhaustedAt: &exhausted},
			{NodeID: "DE-1", TotalBytes: 10},
		},
	})
	if err != nil {
		t.Fatalf("PlanApply() ошибка: %v", err)
	}

	assertQuotaChanges(t, plan.NodeQuotaChanges, []NodeQuotaChange{{NodeID: "NL-1", ExhaustedAt: nil}})

	if len(plan.DesiredChanges) != 2 {
		t.Fatalf("DesiredChanges = %v, ожидались оба access ноды NL-1", changedAccessIDs(plan.DesiredChanges))
	}
	for _, change := range plan.DesiredChanges {
		if change.DesiredState != DesiredStatePresent {
			t.Fatalf("access %s: desired = %v, ожидалось PRESENT", change.AccessID, change.DesiredState)
		}
	}
}

// Изменение лимита, не пересекающее порог, отметок не двигает, но и no-op не
// является: сам лимит периода меняется.
func TestPlanApplyQuotaChangeWithoutThresholdCrossing(t *testing.T) {
	cmd := validCommand()
	cmd.ExpiresAt = tFuture
	cmd.UsageQuotaBytes = 300
	cmd.CommandNumber = 6

	plan, err := PlanApply(ApplyInput{
		Now:         tNow,
		Command:     cmd,
		Entitlement: existingCustomer(tFuture),
		Topology:    exampleTopology(),
		Accesses:    materializedAccesses(DesiredStatePresent, 1),
		OpenPeriod:  &QuotaPeriod{ID: accessID(200), UsageQuotaBytes: 200, StartedAt: tPast},
		NodeUsage: []NodeQuotaUsage{
			{NodeID: "NL-1", TotalBytes: 10},
			{NodeID: "DE-1", TotalBytes: 10},
		},
	})
	if err != nil {
		t.Fatalf("PlanApply() ошибка: %v", err)
	}

	if !plan.UpdatePeriodQuota {
		t.Fatal("новый лимит периода обязан быть записан")
	}
	if plan.IsNoOp() {
		t.Fatal("смена лимита не является no-op")
	}
	if len(plan.NodeQuotaChanges) != 0 || len(plan.DesiredChanges) != 0 || len(plan.TouchedNodes) != 0 {
		t.Fatalf("порог не пересечён, состояние access меняться не должно: %+v", plan)
	}
}

// Рассогласованный с manifest access Apply не трогает ни в какую
// сторону. Проверяются оба источника рассогласования.
func TestPlanApplySkipsAccessesOutOfSyncWithTopology(t *testing.T) {
	tests := []struct {
		name     string
		topology FleetTopology
		accesses []Access
	}{
		{
			name: "цель ушла из manifest",
			// Связь удалена, входная нода NL-1 жива.
			topology: FleetTopology{FleetID: 10, Nodes: []NodeID{"NL-1", "DE-1"}},
			accesses: materializedAccesses(DesiredStatePresent, 1),
		},
		{
			name:     "у связи сменился egress_tag",
			topology: exampleTopology(),
			accesses: func() []Access {
				accesses := materializedAccesses(DesiredStatePresent, 1)
				accesses[2].EgressKey = "de-exit-old"
				return accesses
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Истёкший срок гасит всё, что Apply вправе трогать, — на этом фоне
			// пропуск рассогласованного access виден отчётливо.
			cmd := validCommand()
			cmd.ExpiresAt = tPast
			cmd.CommandNumber = 6

			plan, err := PlanApply(ApplyInput{
				Now:         tNow,
				Command:     cmd,
				Entitlement: existingCustomer(tPast),
				Topology:    tt.topology,
				Accesses:    tt.accesses,
				OpenPeriod:  &QuotaPeriod{ID: accessID(200), UsageQuotaBytes: cmd.UsageQuotaBytes, StartedAt: tPast},
				NodeUsage: []NodeQuotaUsage{
					{NodeID: "NL-1", TotalBytes: 0},
					{NodeID: "DE-1", TotalBytes: 0},
				},
			})
			if err != nil {
				t.Fatalf("PlanApply() ошибка: %v", err)
			}

			for _, change := range plan.DesiredChanges {
				if change.AccessID == accessID(3) {
					t.Fatal("рассогласованный access попал в план операций")
				}
			}
			if len(plan.DesiredChanges) != 2 {
				t.Fatalf("DesiredChanges = %v, ожидались только две согласованные FREEDOM",
					changedAccessIDs(plan.DesiredChanges))
			}
			// Нового access под ту же цель тоже не создаётся: она не пропала, а
			// рассогласована.
			if len(plan.CreateAccesses) != 0 {
				t.Fatalf("CreateAccesses = %v, ожидался пустой список", createLabels(plan.CreateAccesses))
			}
		})
	}
}

// Новая цель, появившаяся пока customer истёк, материализуется сразу ABSENT: у
// неё нет операции, и её нода не считается затронутой.
func TestPlanApplyCreatesBlockedAccessForExpiredCustomer(t *testing.T) {
	cmd := validCommand()
	cmd.ExpiresAt = tPast
	cmd.CommandNumber = 6

	topology := exampleTopology()
	topology.Nodes = append(topology.Nodes, "FR-1")

	plan, err := PlanApply(ApplyInput{
		Now:         tNow,
		Command:     cmd,
		Entitlement: existingCustomer(tPast),
		Topology:    topology,
		Accesses:    materializedAccesses(DesiredStateAbsent, 2),
		OpenPeriod:  &QuotaPeriod{ID: accessID(200), UsageQuotaBytes: cmd.UsageQuotaBytes, StartedAt: tPast},
		NodeUsage: []NodeQuotaUsage{
			{NodeID: "NL-1", TotalBytes: 0},
			{NodeID: "DE-1", TotalBytes: 0},
		},
	})
	if err != nil {
		t.Fatalf("PlanApply() ошибка: %v", err)
	}

	if got := createLabels(plan.CreateAccesses); !slices.Equal(got, []string{"FREEDOM/FR-1"}) {
		t.Fatalf("CreateAccesses = %v, ожидалась только новая нода", got)
	}
	if plan.CreateAccesses[0].DesiredState != DesiredStateAbsent {
		t.Fatalf("desired = %v, у истёкшего customer новый access рождается ABSENT",
			plan.CreateAccesses[0].DesiredState)
	}
	if len(plan.TouchedNodes) != 0 {
		t.Fatalf("TouchedNodes = %v, у ABSENT-access операции нет и состав юзеров ноды не меняется",
			plan.TouchedNodes)
	}
	// Ноде, добавленной в fleet после открытия периода, строку расхода заводит
	// materialization job вместе с её access, а не Apply.
	if len(plan.NodeQuotaInits) != 0 {
		t.Fatalf("NodeQuotaInits = %v, внутри открытого периода Apply строк расхода не заводит",
			plan.NodeQuotaInits)
	}
	if plan.IsNoOp() {
		t.Fatal("создание нового access не является no-op")
	}
}

// Отсутствие открытого периода у существующего customer — нарушение инварианта
// инварианта, а не ошибка вызывающего.
func TestPlanApplyRequiresOpenPeriod(t *testing.T) {
	cmd := validCommand()
	cmd.ExpiresAt = tFuture
	cmd.CommandNumber = 6

	_, err := PlanApply(ApplyInput{
		Now:         tNow,
		Command:     cmd,
		Entitlement: existingCustomer(tFuture),
		Topology:    exampleTopology(),
		Accesses:    materializedAccesses(DesiredStatePresent, 1),
	})
	if !errors.Is(err, ErrOpenPeriodMissing) {
		t.Fatalf("ошибка = %v, ожидалась ErrOpenPeriodMissing", err)
	}
}

// Принятый fleet не удаляется и может быть пустым. Customer при этом
// заводится нормально, просто без единого access.
func TestPlanApplyEmptyFleet(t *testing.T) {
	cmd := validCommand()
	cmd.ExpiresAt = tFuture

	plan, err := PlanApply(ApplyInput{
		Now:      tNow,
		Command:  cmd,
		Topology: FleetTopology{FleetID: 10},
	})
	if err != nil {
		t.Fatalf("PlanApply() ошибка: %v", err)
	}

	if !plan.CreateEntitlement || !plan.OpenNewPeriod {
		t.Fatalf("customer пустого fleet всё равно заводится: %+v", plan)
	}
	if len(plan.CreateAccesses) != 0 || len(plan.TouchedNodes) != 0 || len(plan.NodeQuotaInits) != 0 {
		t.Fatalf("у пустого fleet нет ни access, ни нод: %+v", plan)
	}
	if plan.IsNoOp() {
		t.Fatal("создание customer не является no-op даже при пустом fleet")
	}
}

func TestApplyDecisionString(t *testing.T) {
	tests := []struct {
		decision ApplyDecision
		want     string
	}{
		{decision: ApplyDecisionCreate, want: "CREATE"},
		{decision: ApplyDecisionRenewal, want: "RENEWAL"},
		{decision: ApplyDecisionQuotaChange, want: "QUOTA_CHANGE"},
		{decision: ApplyDecision(0), want: "UNKNOWN"},
	}

	for _, tt := range tests {
		if got := tt.decision.String(); got != tt.want {
			t.Fatalf("ApplyDecision(%d).String() = %q, ожидалось %q", tt.decision, got, tt.want)
		}
	}
}

// Ошибки классификации доходят до вызывающего без плана.
func TestPlanApplyPropagatesClassificationErrors(t *testing.T) {
	cmd := validCommand()
	cmd.FleetID = 11
	cmd.ExpiresAt = tFuture

	_, err := PlanApply(ApplyInput{
		Now:         tNow,
		Command:     cmd,
		Entitlement: existingCustomer(tFuture),
		Topology:    exampleTopology(),
	})
	if !errors.Is(err, ErrFleetMismatch) {
		t.Fatalf("ошибка = %v, ожидалась ErrFleetMismatch", err)
	}
}
