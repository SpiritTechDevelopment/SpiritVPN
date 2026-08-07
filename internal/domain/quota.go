package domain

import (
	"cmp"
	"slices"
	"time"
)

// IsQuotaExhausted — порог исчерпания квоты на ноде. §4 задаёт эффективность
// access через строгое неравенство
//
//	node_quota_period_usage_bytes < usage_quota_bytes
//
// поэтому равенство расхода лимиту уже считается исчерпанием.
func IsQuotaExhausted(totalBytes, quotaBytes uint64) bool {
	return totalBytes >= quotaBytes
}

// NodeQuotaChange — изменение отметки исчерпания на одной ноде.
type NodeQuotaChange struct {
	NodeID NodeID
	// ExhaustedAt: непустое значение ставит отметку, nil снимает её.
	ExhaustedAt *time.Time
}

// RecomputeExhausted пересчитывает exhausted_at под новый лимит в рамках того же
// периода (§5): понижение квоты ставит отметку каждой ноде, где расход достиг
// нового лимита, повышение снимает её только там, где расход снова ниже лимита.
//
// Возвращаются только фактические изменения — ноды, у которых отметка уже
// соответствует лимиту, в план не попадают, иначе точный повтор Apply перестал бы
// быть no-op. Результат отсортирован по node_id: в этом порядке транзакция
// обязана брать row locks (§11.1).
//
// К renewal не применяется: там открывается новый период, где расход нулевой, а
// отметки отсутствуют по построению.
func RecomputeExhausted(usages []NodeQuotaUsage, quotaBytes uint64, now time.Time) []NodeQuotaChange {
	var changes []NodeQuotaChange

	for _, usage := range usages {
		exhausted := IsQuotaExhausted(usage.TotalBytes, quotaBytes)

		switch {
		case exhausted && usage.ExhaustedAt == nil:
			changes = append(changes, NodeQuotaChange{NodeID: usage.NodeID, ExhaustedAt: &now})
		case !exhausted && usage.ExhaustedAt != nil:
			changes = append(changes, NodeQuotaChange{NodeID: usage.NodeID, ExhaustedAt: nil})
		}
	}

	slices.SortFunc(changes, func(a, b NodeQuotaChange) int {
		return cmp.Compare(a.NodeID, b.NodeID)
	})
	return changes
}
