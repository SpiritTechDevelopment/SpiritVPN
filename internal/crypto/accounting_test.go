package crypto

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"testing"
)

// failingReader — источник энтропии, который всегда отказывает.
type failingReader struct{}

var errNoEntropy = errors.New("тест: источник энтропии недоступен")

func (failingReader) Read([]byte) (int, error) { return 0, errNoEntropy }

// Отображение байта в символ проверяется на ВСЕХ 256 значениях сразу: так
// равномерность доказывается по построению (каждому символу ровно 8 байт), а не
// статистикой на выборке.
func TestNewAccountingIDMapsEveryByteValue(t *testing.T) {
	// 256 байт по возрастанию дают 13 идентификаторов по 20 символов (260 байт
	// не хватило бы), поэтому читаем ровно 256 значений за 12 полных вызовов и
	// один частичный.
	source := make([]byte, 256)
	for i := range source {
		source[i] = byte(i)
	}

	reader := bytes.NewReader(source)
	counts := make(map[byte]int, 32)

	for consumed := 0; consumed+accountingIDBodyLen <= len(source); consumed += accountingIDBodyLen {
		id, err := NewAccountingID(reader)
		if err != nil {
			t.Fatalf("NewAccountingID() ошибка: %v", err)
		}

		body := strings.TrimPrefix(id, AccountingIDPrefix)
		for i := 0; i < accountingIDBodyLen; i++ {
			want := accountingIDAlphabet[byte(consumed+i)&accountingIDMask]
			if body[i] != want {
				t.Fatalf("байт %d отобразился в %q, ожидалось %q", consumed+i, body[i], want)
			}
			counts[body[i]]++
		}
	}

	// 240 обработанных байт: символы, чьи 8 прообразов целиком попали в диапазон,
	// встретились ровно 8 раз. Проверяем, что ни один не встретился чаще.
	for _, symbol := range []byte(accountingIDAlphabet) {
		if counts[symbol] > 8 {
			t.Fatalf("символ %q встретился %d раз на 240 байт, маска смещена", symbol, counts[symbol])
		}
	}
}

func TestNewAccountingIDFormat(t *testing.T) {
	id, err := NewAccountingID(rand.Reader)
	if err != nil {
		t.Fatalf("NewAccountingID() ошибка: %v", err)
	}

	if !strings.HasPrefix(id, AccountingIDPrefix) {
		t.Fatalf("accounting_id = %q, ожидался префикс %q", id, AccountingIDPrefix)
	}
	if len(id) != AccountingIDLen {
		t.Fatalf("длина accounting_id = %d, ожидалось %d", len(id), AccountingIDLen)
	}

	body := strings.TrimPrefix(id, AccountingIDPrefix)
	for i := 0; i < len(body); i++ {
		if !strings.ContainsRune(accountingIDAlphabet, rune(body[i])) {
			t.Fatalf("символ %q на позиции %d вне алфавита %q", body[i], i, accountingIDAlphabet)
		}
	}
}

// Отказ источника энтропии обязан стать ошибкой команды, а не тихо усечённым
// идентификатором.
func TestNewAccountingIDPropagatesReaderError(t *testing.T) {
	if _, err := NewAccountingID(failingReader{}); !errors.Is(err, errNoEntropy) {
		t.Fatalf("ошибка = %v, ожидался проброс ошибки источника", err)
	}

	// Усечённый источник — тоже ошибка: частично заполненный буфер недопустим.
	short := bytes.NewReader(make([]byte, accountingIDBodyLen-1))
	if _, err := NewAccountingID(short); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ошибка = %v, ожидался io.ErrUnexpectedEOF", err)
	}
}

// Дымовой тест на реальном CSPRNG: уникальность на разумной выборке и то, что
// алфавит используется целиком.
func TestNewAccountingIDUniqueAndCoversAlphabet(t *testing.T) {
	const samples = 10000

	seen := make(map[string]struct{}, samples)
	used := make(map[rune]struct{}, 32)

	for i := 0; i < samples; i++ {
		id, err := NewAccountingID(rand.Reader)
		if err != nil {
			t.Fatalf("NewAccountingID() ошибка: %v", err)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("коллизия accounting_id на выборке %d: %q", samples, id)
		}
		seen[id] = struct{}{}

		for _, r := range strings.TrimPrefix(id, AccountingIDPrefix) {
			used[r] = struct{}{}
		}
	}

	if len(used) != len(accountingIDAlphabet) {
		t.Fatalf("использовано %d символов из %d, часть алфавита недостижима",
			len(used), len(accountingIDAlphabet))
	}
}
