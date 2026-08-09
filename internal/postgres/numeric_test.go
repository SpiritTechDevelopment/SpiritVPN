package postgres

import (
	"errors"
	"math"
	"math/big"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// TestNumericRoundTrip проверяет, что весь диапазон uint64 переживает упаковку и
// разбор. Границы важнее середины: именно на них int64 обрезал бы значение, ради
// чего колонки и объявлены как numeric(20,0) (§11).
func TestNumericRoundTrip(t *testing.T) {
	values := []uint64{
		0,
		1,
		10000,             // хвостовые нули: драйвер вправе вернуть Int=1, Exp=4
		1 << 30,           // гигабайт квоты
		math.MaxInt64,     // последнее значение, влезающее в bigint
		math.MaxInt64 + 1, // первое, которое bigint уже не вмещает
		math.MaxUint64,    // верхняя граница numeric(20,0)
	}

	for _, want := range values {
		got, err := uint64FromNumeric(numericFromUint64(want))
		if err != nil {
			t.Errorf("uint64FromNumeric(%d): %v", want, err)
			continue
		}
		if got != want {
			t.Errorf("round-trip %d вернул %d", want, got)
		}
	}
}

// TestUint64FromNumericExp покрывает нормализацию Exp: значение numeric равно
// Int * 10^Exp, и драйвер возвращает его в любой эквивалентной форме.
func TestUint64FromNumericExp(t *testing.T) {
	tests := []struct {
		name string
		in   pgtype.Numeric
		want uint64
	}{
		{
			name: "положительный Exp разворачивается",
			in:   pgtype.Numeric{Int: big.NewInt(1), Exp: 4, Valid: true},
			want: 10000,
		},
		{
			name: "отрицательный Exp без дробной части сокращается",
			in:   pgtype.Numeric{Int: big.NewInt(120000), Exp: -2, Valid: true},
			want: 1200,
		},
		{
			name: "нулевой Exp берётся как есть",
			in:   pgtype.Numeric{Int: big.NewInt(42), Exp: 0, Valid: true},
			want: 42,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := uint64FromNumeric(tt.in)
			if err != nil {
				t.Fatalf("uint64FromNumeric: %v", err)
			}
			if got != tt.want {
				t.Fatalf("получено %d, ожидалось %d", got, tt.want)
			}
		})
	}
}

// TestUint64FromNumericRejects проверяет, что значения вне диапазона колонки не
// превращаются молча в мусорное число: схема запрещает их CHECK-ограничениями,
// поэтому их появление — повреждение данных, о котором обязано быть слышно.
func TestUint64FromNumericRejects(t *testing.T) {
	overflow := new(big.Int).Add(new(big.Int).SetUint64(math.MaxUint64), big.NewInt(1))

	tests := []struct {
		name string
		in   pgtype.Numeric
	}{
		{"NULL", pgtype.Numeric{}},
		{"NaN", pgtype.Numeric{NaN: true, Valid: true}},
		{"бесконечность", pgtype.Numeric{InfinityModifier: pgtype.Infinity, Valid: true}},
		{"пустой Int", pgtype.Numeric{Valid: true}},
		{"отрицательное", pgtype.Numeric{Int: big.NewInt(-1), Valid: true}},
		{"дробное", pgtype.Numeric{Int: big.NewInt(15), Exp: -1, Valid: true}},
		{"больше 2^64-1", pgtype.Numeric{Int: overflow, Valid: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := uint64FromNumeric(tt.in)
			if err == nil {
				t.Fatalf("ошибка не возвращена, получено %d", got)
			}
			if !errors.Is(err, ErrNumericOutOfRange) {
				t.Fatalf("ошибка %v не оборачивает ErrNumericOutOfRange", err)
			}
		})
	}
}
