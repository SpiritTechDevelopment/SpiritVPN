package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

// stubRetention — порт ретенции с заданным результатом.
type stubRetention struct {
	deleted int
	err     error

	calls        int
	gotRetention time.Duration
	gotLimit     int
}

func (s *stubRetention) PruneProcessedUsageItems(
	_ context.Context,
	retention time.Duration,
	limit int,
) (int, error) {
	s.calls++
	s.gotRetention, s.gotLimit = retention, limit
	return s.deleted, s.err
}

// Полная пачка означает «работа не закончена»: цикл воркера должен пойти на
// следующий шаг без паузы, иначе уборка отстанет от записи.
func TestPruneUsageDedupReportsMoreWorkOnFullBatch(t *testing.T) {
	repo := &stubRetention{deleted: 100}
	uc := NewPruneUsageDedup(repo, 6*time.Hour, 100)

	progressed, err := uc.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if !progressed {
		t.Error("полная пачка не признана прогрессом — воркер уснёт с непочищенной таблицей")
	}

	if repo.gotRetention != 6*time.Hour || repo.gotLimit != 100 {
		t.Errorf("до порта доехали retention=%v limit=%d, ожидалось 6h и 100",
			repo.gotRetention, repo.gotLimit)
	}
}

// Неполная пачка означает, что удалять больше нечего. Признать её прогрессом
// значило бы крутить цикл без пауз, снося по нескольку строк, только что
// перевалившихся за окно.
func TestPruneUsageDedupSleepsOnPartialBatch(t *testing.T) {
	for _, deleted := range []int{0, 99} {
		repo := &stubRetention{deleted: deleted}
		uc := NewPruneUsageDedup(repo, time.Hour, 100)

		progressed, err := uc.ProcessNext(context.Background())
		if err != nil {
			t.Fatalf("ProcessNext при %d удалённых: %v", deleted, err)
		}
		if progressed {
			t.Errorf("при %d удалённых из 100 шаг признан прогрессом", deleted)
		}
	}
}

func TestPruneUsageDedupPropagatesFailure(t *testing.T) {
	sentinel := errors.New("база недоступна")
	uc := NewPruneUsageDedup(&stubRetention{deleted: 100, err: sentinel}, time.Hour, 100)

	progressed, err := uc.ProcessNext(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("ошибка %v, ожидалась обёрнутая %v", err, sentinel)
	}
	// Существенно: иначе цикл принял бы отказавший шаг за работу и пошёл бы на
	// следующий без backoff, забрасывая недоступную базу запросами.
	if progressed {
		t.Error("отказавший шаг признан прогрессом")
	}
}
