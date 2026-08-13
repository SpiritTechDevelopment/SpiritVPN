package crypto

import (
	"fmt"
	"io"
)

const (
	// AccountingIDPrefix — backend namespace. По нему агент отличает
	// backend-owned users от инфраструктурных `svc-*`.
	AccountingIDPrefix = "u."

	// accountingIDAlphabet — base32 lowercase. Регистр один, потому что
	// accounting_id используется как Xray email и сравнивается дословно.
	accountingIDAlphabet = "abcdefghijklmnopqrstuvwxyz234567"

	// accountingIDBodyLen — длина opaque-части: 20 символов по 5 бит = 100 бит
	// энтропии.
	accountingIDBodyLen = 20

	// accountingIDMask — младшие 5 бит байта. Отображение несмещённое: 256 кратно
	// 32, поэтому каждому символу алфавита соответствует ровно 8 значений байта.
	accountingIDMask = 0x1f

	// AccountingIDLen — полная длина `u.` + 20 символов.
	AccountingIDLen = len(AccountingIDPrefix) + accountingIDBodyLen
)

// Проверка на этапе компиляции: алфавит обязан быть ровно 32 символа, иначе
// маскирование в младшие 5 бит перестаёт быть равномерным.
var _ [0]struct{} = [len(accountingIDAlphabet) - 32]struct{}{}

// NewAccountingID выдаёт стабильный псевдоним access для Xray email и учёта
// трафика.
//
// Значение не содержит customer ID, username, email, телефона и вообще ничего
// пользовательского — это чистый CSPRNG. Глобальная уникальность держится unique
// индексом в БД: при коллизии транзакция обязана упасть, а не ретраить (решение
// 4), иначе тихий ретрай замаскирует сломанный источник энтропии.
func NewAccountingID(random io.Reader) (string, error) {
	buf := make([]byte, accountingIDBodyLen)
	if _, err := io.ReadFull(random, buf); err != nil {
		return "", fmt.Errorf("crypto: чтение CSPRNG для accounting_id: %w", err)
	}

	out := make([]byte, 0, AccountingIDLen)
	out = append(out, AccountingIDPrefix...)
	for _, b := range buf {
		out = append(out, accountingIDAlphabet[b&accountingIDMask])
	}

	return string(out), nil
}
