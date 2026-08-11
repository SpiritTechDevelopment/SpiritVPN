package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeRetention — прунер с заданным результатом.
type fakeRetention struct {
	deleted int
	err     error

	gotRetention time.Duration
	gotLimit     int
}

func (f *fakeRetention) PruneProcessedUsageItems(
	_ context.Context,
	retention time.Duration,
	limit int,
) (int, error) {
	f.gotRetention, f.gotLimit = retention, limit
	return f.deleted, f.err
}

func TestRetentionCountsPrunedRows(t *testing.T) {
	registry := New()
	inner := &fakeRetention{deleted: 17}
	pruner := registry.WrapUsageRetention(inner)

	deleted, err := pruner.PruneProcessedUsageItems(context.Background(), time.Hour, 100)
	if err != nil {
		t.Fatalf("PruneProcessedUsageItems: %v", err)
	}
	if deleted != 17 {
		t.Errorf("удалено %d, ожидалось 17 — декоратор исказил результат", deleted)
	}

	// Аргументы обязаны доехать до порта без изменений: декоратор наблюдает, а не
	// решает, что и как долго хранить.
	if inner.gotRetention != time.Hour || inner.gotLimit != 100 {
		t.Errorf("до порта доехали retention=%v limit=%d, ожидалось 1h и 100",
			inner.gotRetention, inner.gotLimit)
	}

	if got := testutil.ToFloat64(registry.usageDedupPruned); got != 17 {
		t.Errorf("счётчик удалённых %v, ожидалось 17", got)
	}
}

// Отказ шага не считается удалением: часть пачки могла быть удалена только
// вместе с откатом собственной транзакции.
func TestRetentionIgnoresFailedPrune(t *testing.T) {
	registry := New()
	pruner := registry.WrapUsageRetention(&fakeRetention{deleted: 5, err: errors.New("база недоступна")})

	if _, err := pruner.PruneProcessedUsageItems(context.Background(), time.Hour, 100); err == nil {
		t.Fatal("ошибка не проброшена наружу")
	}

	if got := testutil.ToFloat64(registry.usageDedupPruned); got != 0 {
		t.Errorf("счётчик удалённых %v, ожидался 0", got)
	}
}
