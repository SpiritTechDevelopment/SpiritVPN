// Package postgres — адаптер доступа к PostgreSQL: запросы, конверсия типов и
// транзакционная обвязка use case'ов.
//
// Типобезопасный код запросов лежит в подпакете db и генерируется sqlc из
// internal/postgres/queries по схеме из internal/migrations. Руками он не
// правится: источник истины — .sql-файлы.
package postgres

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5/pgtype"
)

// ErrNumericOutOfRange — значение колонки numeric(20,0) не является целым
// беззнаковым числом, помещающимся в uint64.
//
// Схема гарантирует диапазон CHECK-ограничениями: counters неотрицательны,
// поэтому такая ошибка означает повреждение данных или запись мимо backend, а не
// ошибку вызывающего. Отдельный сентинел нужен, чтобы это было видно в логах, а не
// сливалось с ошибками драйвера.
var ErrNumericOutOfRange = errors.New("postgres: numeric не помещается в uint64")

// bigTen переиспользуется как основание при нормализации Exp.
var bigTen = big.NewInt(10)

// numericFromUint64 упаковывает uint64 в numeric(20,0).
//
// Именно numeric, а не bigint: uint64 не помещается в int64, и верхняя половина
// диапазона байтовых counters и command_number обрезалась бы молча.
func numericFromUint64(v uint64) pgtype.Numeric {
	return pgtype.Numeric{
		Int:   new(big.Int).SetUint64(v),
		Exp:   0,
		Valid: true,
	}
}

// uint64FromNumeric разбирает значение колонки numeric(20,0).
//
// Значение numeric равно Int * 10^Exp, и Exp бывает не нулевым даже у колонки со
// scale 0: PostgreSQL передаёт число группами по четыре десятичных цифры, а
// драйвер нормализует результат, отбрасывая хвостовые нули в Exp. С
// проигнорированным Exp 10000 прочиталось бы как 1; на реальных значениях квоты
// это ловится интеграционным тестом round-trip.
func uint64FromNumeric(n pgtype.Numeric) (uint64, error) {
	switch {
	case !n.Valid:
		return 0, fmt.Errorf("%w: NULL", ErrNumericOutOfRange)
	case n.NaN:
		return 0, fmt.Errorf("%w: NaN", ErrNumericOutOfRange)
	case n.InfinityModifier != pgtype.Finite:
		return 0, fmt.Errorf("%w: бесконечность", ErrNumericOutOfRange)
	case n.Int == nil:
		return 0, fmt.Errorf("%w: пустое значение", ErrNumericOutOfRange)
	}

	value := n.Int

	switch {
	case n.Exp > 0:
		value = new(big.Int).Mul(value, new(big.Int).Exp(bigTen, big.NewInt(int64(n.Exp)), nil))

	case n.Exp < 0:
		// Дробная часть недопустима: колонки объявлены со scale 0, а молчаливое
		// округление байтов трафика исказило бы учёт квоты.
		scale := new(big.Int).Exp(bigTen, big.NewInt(int64(-n.Exp)), nil)
		quotient, remainder := new(big.Int).QuoRem(value, scale, new(big.Int))
		if remainder.Sign() != 0 {
			return 0, fmt.Errorf("%w: дробное значение", ErrNumericOutOfRange)
		}
		value = quotient
	}

	if value.Sign() < 0 {
		return 0, fmt.Errorf("%w: отрицательное значение", ErrNumericOutOfRange)
	}
	if !value.IsUint64() {
		return 0, fmt.Errorf("%w: больше 2^64-1", ErrNumericOutOfRange)
	}

	return value.Uint64(), nil
}
