package domain

import (
	"errors"
	"slices"
	"testing"
)

// projectionOf собирает проекцию так, как она выглядела бы после успешного
// приёма снапшота. Тесты ниже сравнивают следующий снапшот именно с ней.
func projectionOf(s ManifestSnapshot) ManifestProjection {
	_, digest := CanonicalizeManifest(s)

	projection := ManifestProjection{
		LastRevision: s.Revision,
		LastDigest:   digest,
	}
	for _, node := range s.Nodes {
		projection.CurrentNodes = append(projection.CurrentNodes, node.NodeID)
	}
	for _, fleet := range s.Fleets {
		projection.Fleets = append(projection.Fleets, fleet.FleetID)
		for _, nodeID := range fleet.NodeIDs {
			projection.CurrentMemberships = append(projection.CurrentMemberships,
				FleetNodeKey{FleetID: fleet.FleetID, NodeID: nodeID})
		}
		for _, bridge := range fleet.Bridges {
			projection.Bridges = append(projection.Bridges, ProjectedBridge{
				BridgeKey:   BridgeKey{FleetID: fleet.FleetID, RoutingKey: bridge.RoutingKey},
				EntryNodeID: bridge.EntryNodeID,
				ExitNodeID:  bridge.ExitNodeID,
				Current:     true,
			})
		}
	}
	return projection
}

// next возвращает снапшот со следующей revision — обычный случай приёма.
func next(s ManifestSnapshot) ManifestSnapshot {
	s.Revision++
	return s
}

func planOrFail(t *testing.T, in ManifestInput) ManifestPlan {
	t.Helper()

	plan, err := PlanManifest(in)
	if err != nil {
		t.Fatalf("PlanManifest: %v", err)
	}
	return plan
}

// TestPlanManifestFirstAcceptance — самый первый манифест: проекция пуста,
// удалять нечего, destructive-флаг не нужен.
func TestPlanManifestFirstAcceptance(t *testing.T) {
	snapshot := validSnapshot()
	plan := planOrFail(t, ManifestInput{Snapshot: snapshot})

	if plan.Idempotent || plan.Destructive {
		t.Fatalf("план %+v, ожидался обычный приём", plan)
	}
	if plan.Revision != snapshot.Revision {
		t.Errorf("revision %d, ожидалась %d", plan.Revision, snapshot.Revision)
	}
	if len(plan.Payload) == 0 || plan.Digest == "" {
		t.Error("план не несёт канонический payload и digest")
	}

	if len(plan.Nodes) != 2 || len(plan.FleetIDs) != 1 || len(plan.Memberships) != 2 || len(plan.Bridges) != 1 {
		t.Fatalf("состав плана: нод %d, fleets %d, membership %d, связей %d",
			len(plan.Nodes), len(plan.FleetIDs), len(plan.Memberships), len(plan.Bridges))
	}

	// Ноды упорядочены по node_id: в этом порядке транзакция берёт их locks.
	if got := []NodeID{plan.Nodes[0].NodeID, plan.Nodes[1].NodeID}; !slices.Equal(got, []NodeID{"DE-1", "NL-1"}) {
		t.Errorf("порядок нод %v, ожидался [DE-1 NL-1]", got)
	}
	if !slices.IsSorted(plan.FleetIDs) {
		t.Errorf("fleets не отсортированы: %v", plan.FleetIDs)
	}
	if !slices.IsSortedFunc(plan.Memberships, compareFleetNodeKey) {
		t.Errorf("membership не отсортированы: %v", plan.Memberships)
	}
}

// TestPlanManifestIdempotentReplay — повтор той же revision с тем же digest.
// План пуст целиком, писать нечего вовсе.
func TestPlanManifestIdempotentReplay(t *testing.T) {
	snapshot := validSnapshot()
	plan := planOrFail(t, ManifestInput{Snapshot: snapshot, Projection: projectionOf(snapshot)})

	if !plan.Idempotent {
		t.Fatal("повтор той же revision с тем же digest не признан идемпотентным")
	}
	if len(plan.Nodes) != 0 || len(plan.FleetIDs) != 0 || len(plan.Payload) != 0 {
		t.Fatalf("идемпотентный план несёт записи: %+v", plan)
	}
}

// TestPlanManifestIdempotentIgnoresInputOrder — повтор от CI с другим порядком
// обхода остаётся повтором: digest считается по канонической форме.
func TestPlanManifestIdempotentIgnoresInputOrder(t *testing.T) {
	accepted := validSnapshot()

	replay := validSnapshot()
	slices.Reverse(replay.Nodes)

	plan := planOrFail(t, ManifestInput{Snapshot: replay, Projection: projectionOf(accepted)})
	if !plan.Idempotent {
		t.Fatal("повтор с другим порядком нод не признан идемпотентным")
	}
}

func TestPlanManifestRevisionGate(t *testing.T) {
	accepted := validSnapshot()
	projection := projectionOf(accepted)

	tests := []struct {
		name     string
		snapshot func() ManifestSnapshot
		want     error
	}{
		{
			name: "та же revision с другим содержимым",
			snapshot: func() ManifestSnapshot {
				s := validSnapshot()
				s.Nodes[0].Public.Address = "moved.example.com"
				return s
			},
			want: ErrManifestDigestConflict,
		},
		{
			name: "более старая revision",
			snapshot: func() ManifestSnapshot {
				s := validSnapshot()
				s.Revision--
				return s
			},
			want: ErrManifestRevisionRegression,
		},
		{
			// Правил два, и более специальное про старую revision
			// перекрывает общее про идемпотентный повтор.
			name: "старая revision с совпадающим содержимым",
			snapshot: func() ManifestSnapshot {
				s := validSnapshot()
				s.Revision = 1
				return s
			},
			want: ErrManifestRevisionRegression,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PlanManifest(ManifestInput{Snapshot: tc.snapshot(), Projection: projection})
			if !errors.Is(err, tc.want) {
				t.Fatalf("ошибка %v, ожидалась %v", err, tc.want)
			}
		})
	}
}

// TestPlanManifestRollback — rollback выполняется прежним desired snapshot
// под новой большей revision. Он обязан проходить как обычный приём.
func TestPlanManifestRollback(t *testing.T) {
	original := validSnapshot()

	changed := next(validSnapshot())
	changed.Nodes[0].Public.Address = "moved.example.com"

	rollback := original
	rollback.Revision = changed.Revision + 1

	plan := planOrFail(t, ManifestInput{Snapshot: rollback, Projection: projectionOf(changed)})
	if plan.Idempotent || plan.Destructive {
		t.Fatalf("rollback %+v, ожидался обычный приём", plan)
	}
}

// TestPlanManifestFleetMissing — отсутствие ранее принятого fleet отклоняет
// весь манифест независимо от allow_destructive.
func TestPlanManifestFleetMissing(t *testing.T) {
	accepted := validSnapshot()

	snapshot := next(validSnapshot())
	snapshot.Fleets = nil

	for _, allow := range []bool{false, true} {
		_, err := PlanManifest(ManifestInput{
			Snapshot:         snapshot,
			AllowDestructive: allow,
			Projection:       projectionOf(accepted),
		})
		if !errors.Is(err, ErrManifestFleetMissing) {
			t.Fatalf("allow_destructive=%v: ошибка %v, ожидалась ErrManifestFleetMissing", allow, err)
		}
	}
}

// TestPlanManifestBridgePairImmutable — пара (entry, exit) неизменяема для
// существующего routing_key.
func TestPlanManifestBridgePairImmutable(t *testing.T) {
	accepted := validSnapshot()
	accepted.Nodes = append(accepted.Nodes, manifestNode("FR-1"))
	accepted.Fleets[0].NodeIDs = append(accepted.Fleets[0].NodeIDs, "FR-1")

	// Проекция снимается ДО правки снапшота: next копирует структуру, но слайс
	// Fleets у копии общий с оригиналом, и запись в его элемент видна обоим.
	projection := projectionOf(accepted)

	snapshot := next(accepted)
	snapshot.Fleets[0].Bridges = []ManifestBridge{{
		RoutingKey:  "nl-1.to-de-1", // тот же ключ
		EntryNodeID: "NL-1",
		ExitNodeID:  "FR-1", // другой конец
		EgressTag:   "fr-exit",
	}}

	_, err := PlanManifest(ManifestInput{
		Snapshot:         snapshot,
		AllowDestructive: true,
		Projection:       projection,
	})
	if !errors.Is(err, ErrManifestBridgePairImmutable) {
		t.Fatalf("ошибка %v, ожидалась ErrManifestBridgePairImmutable", err)
	}
}

// TestPlanManifestBridgePairImmutableForRemoved — строка связи живёт вечно,
// поэтому оживление удалённого routing_key с другой парой тоже запрещено.
func TestPlanManifestBridgePairImmutableForRemoved(t *testing.T) {
	accepted := validSnapshot()
	accepted.Nodes = append(accepted.Nodes, manifestNode("FR-1"))
	accepted.Fleets[0].NodeIDs = append(accepted.Fleets[0].NodeIDs, "FR-1")

	projection := projectionOf(accepted)
	// Связь уже была удалена прошлым манифестом.
	projection.Bridges[0].Current = false

	snapshot := next(accepted)
	snapshot.Fleets[0].Bridges[0].ExitNodeID = "FR-1"
	snapshot.Fleets[0].Bridges[0].EgressTag = "fr-exit"

	_, err := PlanManifest(ManifestInput{
		Snapshot:         snapshot,
		AllowDestructive: true,
		Projection:       projection,
	})
	if !errors.Is(err, ErrManifestBridgePairImmutable) {
		t.Fatalf("ошибка %v, ожидалась ErrManifestBridgePairImmutable", err)
	}
}

// TestPlanManifestDestructiveGate — три вида удаления требуют
// allow_destructive; без него манифест отклоняется.
func TestPlanManifestDestructiveGate(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*ManifestSnapshot)
		wantPlan func(*testing.T, ManifestPlan)
	}{
		{
			name: "удалена связь",
			mutate: func(s *ManifestSnapshot) {
				s.Fleets[0].Bridges = nil
			},
			wantPlan: func(t *testing.T, plan ManifestPlan) {
				if len(plan.RemovedBridges) != 1 || plan.RemovedBridges[0].RoutingKey != "nl-1.to-de-1" {
					t.Errorf("удалённые связи %v", plan.RemovedBridges)
				}
			},
		},
		{
			name: "нода вышла из fleet",
			mutate: func(s *ManifestSnapshot) {
				s.Fleets[0].NodeIDs = []NodeID{"NL-1"}
				s.Fleets[0].Bridges = nil
			},
			wantPlan: func(t *testing.T, plan ManifestPlan) {
				want := []FleetNodeKey{{FleetID: 10, NodeID: "DE-1"}}
				if !slices.Equal(plan.RemovedMemberships, want) {
					t.Errorf("удалённые membership %v, ожидались %v", plan.RemovedMemberships, want)
				}
				// Сама нода осталась в манифесте: глобального удаления нет.
				if len(plan.RemovedNodes) != 0 {
					t.Errorf("нода удалена глобально: %v", plan.RemovedNodes)
				}
			},
		},
		{
			name: "нода удалена глобально",
			mutate: func(s *ManifestSnapshot) {
				s.Nodes = []ManifestNode{manifestNode("NL-1")}
				s.Fleets[0].NodeIDs = []NodeID{"NL-1"}
				s.Fleets[0].Bridges = nil
			},
			wantPlan: func(t *testing.T, plan ManifestPlan) {
				if !slices.Equal(plan.RemovedNodes, []NodeID{"DE-1"}) {
					t.Errorf("удалённые ноды %v, ожидалась DE-1", plan.RemovedNodes)
				}
			},
		},
	}

	accepted := validSnapshot()
	projection := projectionOf(accepted)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := next(validSnapshot())
			tc.mutate(&snapshot)

			_, err := PlanManifest(ManifestInput{Snapshot: snapshot, Projection: projection})
			if !errors.Is(err, ErrManifestDestructive) {
				t.Fatalf("без allow_destructive: ошибка %v, ожидалась ErrManifestDestructive", err)
			}

			plan := planOrFail(t, ManifestInput{
				Snapshot:         snapshot,
				AllowDestructive: true,
				Projection:       projection,
			})
			if !plan.Destructive {
				t.Error("план не помечен destructive — аудит не увидит удаления")
			}
			tc.wantPlan(t, plan)
		})
	}
}

// TestPlanManifestAdditionsAreNotDestructive — добавление объектов и
// изменение публичных параметров destructive-флага не требуют.
func TestPlanManifestAdditionsAreNotDestructive(t *testing.T) {
	accepted := validSnapshot()

	snapshot := next(validSnapshot())
	snapshot.Nodes = append(snapshot.Nodes, manifestNode("FR-1"))
	snapshot.Fleets[0].NodeIDs = append(snapshot.Fleets[0].NodeIDs, "FR-1")
	snapshot.Fleets = append(snapshot.Fleets, ManifestFleet{FleetID: 11})
	snapshot.Nodes[0].Public.Address = "moved.example.com"
	snapshot.Nodes[0].Agent.Endpoint = "10.0.0.99:9443"

	plan := planOrFail(t, ManifestInput{Snapshot: snapshot, Projection: projectionOf(accepted)})
	if plan.Destructive {
		t.Fatalf("добавления помечены destructive: %+v", plan)
	}
}

// TestPlanManifestRepointIsNotDestructive — смена egress_tag при неизменных
// routing_key и паре — repoint, а не удаление.
func TestPlanManifestRepointIsNotDestructive(t *testing.T) {
	accepted := validSnapshot()

	snapshot := next(validSnapshot())
	snapshot.Fleets[0].Bridges[0].EgressTag = "de-exit-v2"

	plan := planOrFail(t, ManifestInput{Snapshot: snapshot, Projection: projectionOf(accepted)})
	if plan.Destructive {
		t.Fatal("repoint помечен destructive")
	}
	if plan.Bridges[0].EgressTag != "de-exit-v2" {
		t.Fatalf("egress_tag %q, ожидался de-exit-v2", plan.Bridges[0].EgressTag)
	}
}

// TestPlanManifestRevive — повторно добавленная нода оживляет свою
// строку. Удалением это не является и destructive-флага не требует.
func TestPlanManifestRevive(t *testing.T) {
	accepted := validSnapshot()

	projection := projectionOf(accepted)
	// DE-1 была удалена прошлым манифестом: строка есть, но не текущая.
	projection.CurrentNodes = []NodeID{"NL-1"}
	projection.CurrentMemberships = []FleetNodeKey{{FleetID: 10, NodeID: "NL-1"}}
	projection.Bridges[0].Current = false

	snapshot := next(accepted)

	plan := planOrFail(t, ManifestInput{Snapshot: snapshot, Projection: projection})
	if plan.Destructive {
		t.Fatalf("возврат ноды помечен destructive: %+v", plan)
	}
	if len(plan.Nodes) != 2 || len(plan.Bridges) != 1 {
		t.Fatalf("оживлённые строки не попали в план: %+v", plan)
	}
}

// TestPlanManifestRouteTransfer — разрешает перенос route: удалить старый
// routing_key и добавить новый. Пара при этом освобождается, потому что
// уникальность пары держится только на текущих связях (см. baseline).
func TestPlanManifestRouteTransfer(t *testing.T) {
	accepted := validSnapshot()

	snapshot := next(validSnapshot())
	snapshot.Fleets[0].Bridges = []ManifestBridge{{
		RoutingKey:  "nl-1.to-de-1.v2",
		EntryNodeID: "NL-1",
		ExitNodeID:  "DE-1",
		EgressTag:   "de-exit",
	}}

	if err := ValidateManifest(snapshot); err != nil {
		t.Fatalf("снапшот с перенесённым route отвергнут валидацией: %v", err)
	}

	plan := planOrFail(t, ManifestInput{
		Snapshot:         snapshot,
		AllowDestructive: true,
		Projection:       projectionOf(accepted),
	})

	if len(plan.RemovedBridges) != 1 || plan.RemovedBridges[0].RoutingKey != "nl-1.to-de-1" {
		t.Errorf("удалённые связи %v", plan.RemovedBridges)
	}
	if len(plan.Bridges) != 1 || plan.Bridges[0].RoutingKey != "nl-1.to-de-1.v2" {
		t.Errorf("текущие связи %v", plan.Bridges)
	}
}
