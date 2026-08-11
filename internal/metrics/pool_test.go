package metrics

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestPoolCollectorReportsWithoutConnecting — коллектор обязан отдавать метрики
// даже когда база недоступна.
//
// Это не искусственный случай, а главный: /metrics нужнее всего именно тогда,
// когда PostgreSQL не отвечает, и коллектор, ждущий соединения, отнимал бы
// наблюдаемость ровно в этот момент. pgxpool.New соединяется лениво, поэтому пул
// к несуществующему адресу — корректный способ это проверить.
func TestPoolCollectorReportsWithoutConnecting(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/nothing?sslmode=disable")
	if err != nil {
		t.Fatalf("создание пула: %v", err)
	}
	defer pool.Close()

	registry := New()
	registry.RegisterPool(pool)

	families, err := registry.reg.Gather()
	if err != nil {
		t.Fatalf("сбор метрик: %v", err)
	}

	found := map[string]bool{}
	for _, f := range families {
		if strings.HasPrefix(f.GetName(), namespace+"_db_pool_") {
			found[f.GetName()] = true
		}
	}

	for _, want := range []string{
		namespace + "_db_pool_connections",
		namespace + "_db_pool_max_connections",
		namespace + "_db_pool_acquires_total",
		namespace + "_db_pool_acquire_wait_seconds_total",
	} {
		if !found[want] {
			t.Errorf("метрики %s нет в выдаче", want)
		}
	}
}

// TestPoolCollectorLabelsAreAllowed — коллектор пула объявляет метку строкой, а
// не константой пакета, поэтому в белый список §15 она попадает отдельно.
func TestPoolCollectorLabelsAreAllowed(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/nothing?sslmode=disable")
	if err != nil {
		t.Fatalf("создание пула: %v", err)
	}
	defer pool.Close()

	registry := New()
	registry.RegisterPool(pool)

	families, err := registry.reg.Gather()
	if err != nil {
		t.Fatalf("сбор метрик: %v", err)
	}

	var states []string
	for _, f := range families {
		if f.GetName() != namespace+"_db_pool_connections" {
			continue
		}
		for _, metric := range f.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() != "state" {
					t.Errorf("неожиданная метка %q у метрики пула", label.GetName())
				}
				states = append(states, label.GetValue())
			}
		}
	}

	if len(states) != 4 {
		t.Errorf("состояний соединений %d (%v), ожидалось 4", len(states), states)
	}
}
