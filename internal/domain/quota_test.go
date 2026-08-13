package domain

import (
	"testing"
	"time"
)

// Эффективный access требует total < quota, поэтому равенство уже является
// исчерпанием.
func TestIsQuotaExhausted(t *testing.T) {
	tests := []struct {
		name  string
		total uint64
		quota uint64
		want  bool
	}{
		{name: "расход ниже лимита", total: 99, quota: 100, want: false},
		{name: "расход равен лимиту — граница исчерпания", total: 100, quota: 100, want: true},
		{name: "расход выше лимита", total: 101, quota: 100, want: true},
		{name: "нулевой расход", total: 0, quota: 100, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsQuotaExhausted(tt.total, tt.quota); got != tt.want {
				t.Fatalf("IsQuotaExhausted(%d, %d) = %v, ожидалось %v", tt.total, tt.quota, got, tt.want)
			}
		})
	}
}

func TestRecomputeExhausted(t *testing.T) {
	exhaustedEarlier := tPast

	tests := []struct {
		name   string
		usages []NodeQuotaUsage
		quota  uint64
		want   []NodeQuotaChange
	}{
		{
			name: "понижение квоты ставит отметку там, где расход достиг лимита",
			usages: []NodeQuotaUsage{
				{NodeID: "NL-1", TotalBytes: 150},
				{NodeID: "DE-1", TotalBytes: 50},
			},
			quota: 100,
			want:  []NodeQuotaChange{{NodeID: "NL-1", ExhaustedAt: &tNow}},
		},
		{
			name: "повышение квоты снимает отметку там, где расход снова ниже лимита",
			usages: []NodeQuotaUsage{
				{NodeID: "NL-1", TotalBytes: 150, ExhaustedAt: &exhaustedEarlier},
			},
			quota: 200,
			want:  []NodeQuotaChange{{NodeID: "NL-1", ExhaustedAt: nil}},
		},
		{
			name: "уже согласованное состояние изменений не даёт",
			usages: []NodeQuotaUsage{
				{NodeID: "NL-1", TotalBytes: 150, ExhaustedAt: &exhaustedEarlier},
				{NodeID: "DE-1", TotalBytes: 10},
			},
			quota: 100,
			want:  nil,
		},
		{
			name: "исчерпание одной ноды не затрагивает другую",
			usages: []NodeQuotaUsage{
				{NodeID: "NL-1", TotalBytes: 100},
				{NodeID: "DE-1", TotalBytes: 99},
			},
			quota: 100,
			want:  []NodeQuotaChange{{NodeID: "NL-1", ExhaustedAt: &tNow}},
		},
		{
			name: "результат отсортирован по node_id — нормативный порядок блокировок",
			usages: []NodeQuotaUsage{
				{NodeID: "NL-1", TotalBytes: 100},
				{NodeID: "DE-1", TotalBytes: 100},
				{NodeID: "FR-1", TotalBytes: 100},
			},
			quota: 100,
			want: []NodeQuotaChange{
				{NodeID: "DE-1", ExhaustedAt: &tNow},
				{NodeID: "FR-1", ExhaustedAt: &tNow},
				{NodeID: "NL-1", ExhaustedAt: &tNow},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RecomputeExhausted(tt.usages, tt.quota, tNow)
			assertQuotaChanges(t, got, tt.want)
		})
	}
}

func assertQuotaChanges(t *testing.T, got, want []NodeQuotaChange) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("изменений = %d (%+v), ожидалось %d (%+v)", len(got), got, len(want), want)
	}
	for i := range got {
		if got[i].NodeID != want[i].NodeID {
			t.Fatalf("изменение %d: node_id = %q, ожидалось %q", i, got[i].NodeID, want[i].NodeID)
		}
		if !equalTimePtr(got[i].ExhaustedAt, want[i].ExhaustedAt) {
			t.Fatalf("изменение %d (%s): exhausted_at = %v, ожидалось %v",
				i, got[i].NodeID, got[i].ExhaustedAt, want[i].ExhaustedAt)
		}
	}
}

func equalTimePtr(a, b *time.Time) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return a.Equal(*b)
	}
}
