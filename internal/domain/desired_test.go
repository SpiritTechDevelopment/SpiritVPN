package domain

import (
	"slices"
	"testing"
	"time"
)

// Эффективный access требует одновременно неистёкшего срока и неисчерпанной
// квоты его ноды.
func TestDesiredStateFor(t *testing.T) {
	tests := []struct {
		name      string
		expiresAt time.Time
		exhausted bool
		want      DesiredState
	}{
		{
			name:      "срок не истёк, квота свободна",
			expiresAt: tFuture,
			exhausted: false,
			want:      DesiredStatePresent,
		},
		{
			name:      "срок истёк",
			expiresAt: tPast,
			exhausted: false,
			want:      DesiredStateAbsent,
		},
		{
			name:      "квота ноды исчерпана",
			expiresAt: tFuture,
			exhausted: true,
			want:      DesiredStateAbsent,
		},
		{
			name:      "истёк срок и исчерпана квота",
			expiresAt: tPast,
			exhausted: true,
			want:      DesiredStateAbsent,
		},
		{
			name:      "момент окончания ровно now — доступ уже неэффективен",
			expiresAt: tNow,
			exhausted: false,
			want:      DesiredStateAbsent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DesiredStateFor(tNow, tt.expiresAt, tt.exhausted); got != tt.want {
				t.Fatalf("DesiredStateFor() = %v, ожидалось %v", got, tt.want)
			}
		})
	}
}

func TestPlanDesiredChanges(t *testing.T) {
	presentOnNL := Access{
		ID: accessID(1), Kind: AccessKindFreedom, LogicalTargetKey: "NL-1",
		EntryNodeID: "NL-1", DesiredState: DesiredStatePresent, DesiredVersion: 4,
	}
	presentOnDE := Access{
		ID: accessID(2), Kind: AccessKindFreedom, LogicalTargetKey: "DE-1",
		EntryNodeID: "DE-1", DesiredState: DesiredStatePresent, DesiredVersion: 1,
	}
	absentOnNL := Access{
		ID: accessID(3), Kind: AccessKindBridge, LogicalTargetKey: "nl-1.to-de-1",
		EntryNodeID: "NL-1", DesiredState: DesiredStateAbsent, DesiredVersion: 7,
	}

	tests := []struct {
		name      string
		accesses  []Access
		expiresAt time.Time
		exhausted map[NodeID]bool
		want      []DesiredChange
	}{
		{
			name:      "неизменившийся кортеж версию не двигает",
			accesses:  []Access{presentOnNL, presentOnDE},
			expiresAt: tFuture,
			want:      nil,
		},
		{
			name:      "истечение срока гасит все ноды сразу",
			accesses:  []Access{presentOnNL, presentOnDE},
			expiresAt: tPast,
			want: []DesiredChange{
				{AccessID: accessID(1), EntryNodeID: "NL-1", DesiredState: DesiredStateAbsent, DesiredVersion: 5},
				{AccessID: accessID(2), EntryNodeID: "DE-1", DesiredState: DesiredStateAbsent, DesiredVersion: 2},
			},
		},
		{
			name:      "исчерпание квоты гасит только свою ноду",
			accesses:  []Access{presentOnNL, presentOnDE},
			expiresAt: tFuture,
			exhausted: map[NodeID]bool{"NL-1": true},
			want: []DesiredChange{
				{AccessID: accessID(1), EntryNodeID: "NL-1", DesiredState: DesiredStateAbsent, DesiredVersion: 5},
			},
		},
		{
			name:      "снятие блокировки поднимает access обратно",
			accesses:  []Access{absentOnNL},
			expiresAt: tFuture,
			want: []DesiredChange{
				{AccessID: accessID(3), EntryNodeID: "NL-1", DesiredState: DesiredStatePresent, DesiredVersion: 8},
			},
		},
		{
			name: "результат отсортирован по access_id — нормативный порядок блокировок",
			accesses: []Access{
				{ID: accessID(9), EntryNodeID: "NL-1", DesiredState: DesiredStatePresent},
				{ID: accessID(2), EntryNodeID: "NL-1", DesiredState: DesiredStatePresent},
				{ID: accessID(5), EntryNodeID: "NL-1", DesiredState: DesiredStatePresent},
			},
			expiresAt: tPast,
			want: []DesiredChange{
				{AccessID: accessID(2), EntryNodeID: "NL-1", DesiredState: DesiredStateAbsent, DesiredVersion: 1},
				{AccessID: accessID(5), EntryNodeID: "NL-1", DesiredState: DesiredStateAbsent, DesiredVersion: 1},
				{AccessID: accessID(9), EntryNodeID: "NL-1", DesiredState: DesiredStateAbsent, DesiredVersion: 1},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PlanDesiredChanges(tt.accesses, tNow, tt.expiresAt, tt.exhausted)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("PlanDesiredChanges() = %+v, ожидалось %+v", got, tt.want)
			}
		})
	}
}
