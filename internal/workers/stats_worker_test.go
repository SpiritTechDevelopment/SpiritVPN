package workers

import (
	"context"
	"testing"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/database"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// MockXrayStatsClient реализует интерфейс XrayStatsClient для тестирования.
type MockXrayStatsClient struct {
	mock.Mock
}

func (m *MockXrayStatsClient) GetStats(ctx context.Context, email string) (int64, int64, error) {
	args := m.Called(ctx, email)
	return args.Get(0).(int64), args.Get(1).(int64), args.Error(2)
}

// Проверка что мок реализует интерфейс
var _ XrayStatsClient = (*MockXrayStatsClient)(nil)

// TestNewStatsWorker проверяет создание нового воркера.
// Тестирует корректную инициализацию всех полей структуры.
func TestNewStatsWorker(t *testing.T) {
	mockClient := new(MockXrayStatsClient)
	mockDB := &database.DB{} // Используем пустой DB для теста (без реального подключения)
	interval := 5 * time.Minute

	worker := NewStatsWorker(mockClient, mockDB, interval)

	assert.NotNil(t, worker)
	assert.Equal(t, mockClient, worker.xrayClient)
	assert.Equal(t, mockDB, worker.db)
	assert.Equal(t, interval, worker.interval)
	assert.NotNil(t, worker.log)
}

// TestStatsWorker_Start проверяет запуск и остановку воркера.
// Тестирует что воркер корректно стартует и останавливается по контексту.
// Использует nil DB чтобы избежать реальных обращений к БД.
func TestStatsWorker_Start(t *testing.T) {
	tests := []struct {
		name        string
		interval    time.Duration
		cancelAfter time.Duration
	}{
		{
			name:        "worker stops immediately on context cancel",
			interval:    100 * time.Millisecond,
			cancelAfter: 10 * time.Millisecond,
		},
		{
			name:        "worker stops on context cancel after delay",
			interval:    1 * time.Second,
			cancelAfter: 100 * time.Millisecond,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := new(MockXrayStatsClient)
			worker := NewStatsWorker(mockClient, nil, tt.interval)

			ctx, cancel := context.WithCancel(context.Background())

			// Запуск воркера в корутине
			done := make(chan struct{})
			go func() {
				worker.Start(ctx)
				close(done)
			}()

			time.Sleep(tt.cancelAfter)

			cancel()

			select {
			case <-done:
			case <-time.After(1 * time.Second):
				t.Fatal("Worker did not stop after context cancellation")
			}
		})
	}
}

// TestStatsWorker_Start_ContextCancelledBeforeStart проверяет поведение
// когда контекст отменен до запуска воркера.
func TestStatsWorker_Start_ContextCancelledBeforeStart(t *testing.T) {
	mockClient := new(MockXrayStatsClient)

	worker := NewStatsWorker(mockClient, nil, 1*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		worker.Start(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Worker should stop immediately with cancelled context")
	}
}

// TestXrayStatsClient_Interface проверяет что наш мок корректно реализует интерфейс.
// Это compile-time проверка через переменную уровня пакета.
func TestXrayStatsClient_Interface(t *testing.T) {
	var _ XrayStatsClient = (*MockXrayStatsClient)(nil)
}
