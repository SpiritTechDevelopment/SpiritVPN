package app_test

import (
	"testing"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
)

// Проверка на этапе компиляции: адаптеры действительно закрывают порты. Ссылки на
// конкретные реализации живут в тесте, чтобы сам пакет app от адаптеров не
// зависел.
var (
	_ app.IDs              = (*crypto.Generator)(nil)
	_ app.CredentialSealer = (*crypto.Cipher)(nil)
	_ app.Clock            = app.SystemClock{}
)

// Часы процесса отдают UTC: вся схема и внешний контракт оперируют UTC (§5, §11).
func TestSystemClockReturnsUTC(t *testing.T) {
	before := time.Now().UTC()
	got := app.SystemClock{}.Now()
	after := time.Now().UTC()

	if got.Location() != time.UTC {
		t.Fatalf("Now() вернул зону %v, ожидался UTC", got.Location())
	}
	if got.Before(before) || got.After(after) {
		t.Fatalf("Now() = %v вне диапазона [%v, %v]", got, before, after)
	}
}
