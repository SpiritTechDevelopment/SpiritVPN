package crypto

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestGeneratorProducesDistinctValues(t *testing.T) {
	g := NewGenerator()

	const samples = 512
	accessIDs := make(map[uuid.UUID]struct{}, samples)
	clientUUIDs := make(map[uuid.UUID]struct{}, samples)

	for i := 0; i < samples; i++ {
		accessID, err := g.NewAccessID()
		if err != nil {
			t.Fatalf("NewAccessID() ошибка: %v", err)
		}
		periodID, err := g.NewQuotaPeriodID()
		if err != nil {
			t.Fatalf("NewQuotaPeriodID() ошибка: %v", err)
		}
		operationID, err := g.NewOperationID()
		if err != nil {
			t.Fatalf("NewOperationID() ошибка: %v", err)
		}
		clientUUID, err := g.NewClientUUID()
		if err != nil {
			t.Fatalf("NewClientUUID() ошибка: %v", err)
		}

		if operationID.Version() != 4 {
			t.Fatal("ожидался UUIDv4 для operation_id")
		}
		if accessID.Version() != 4 || periodID.Version() != 4 || clientUUID.Reveal().Version() != 4 {
			t.Fatal("ожидался UUIDv4 (§7)")
		}
		if _, dup := accessIDs[accessID]; dup {
			t.Fatalf("коллизия access_id: %v", accessID)
		}
		if _, dup := clientUUIDs[clientUUID.Reveal()]; dup {
			t.Fatal("коллизия client_uuid")
		}
		accessIDs[accessID] = struct{}{}
		clientUUIDs[clientUUID.Reveal()] = struct{}{}
	}
}

func TestGeneratorAccountingID(t *testing.T) {
	id, err := NewGenerator().NewAccountingID()
	if err != nil {
		t.Fatalf("NewAccountingID() ошибка: %v", err)
	}
	if len(id) != AccountingIDLen || !strings.HasPrefix(id, AccountingIDPrefix) {
		t.Fatalf("accounting_id = %q", id)
	}
}

// Отказ CSPRNG обязан стать ошибкой команды: uuid.New паникует, поэтому под
// капотом используется uuid.NewRandomFromReader (§7, решение 4).
func TestGeneratorPropagatesRandomError(t *testing.T) {
	g := newGeneratorWithRandom(failingReader{})

	if _, err := g.NewAccessID(); !errors.Is(err, errNoEntropy) {
		t.Fatalf("NewAccessID() = %v, ожидался проброс ошибки источника", err)
	}
	if _, err := g.NewQuotaPeriodID(); !errors.Is(err, errNoEntropy) {
		t.Fatalf("NewQuotaPeriodID() = %v, ожидался проброс ошибки источника", err)
	}
	if _, err := g.NewOperationID(); !errors.Is(err, errNoEntropy) {
		t.Fatalf("NewOperationID() = %v, ожидался проброс ошибки источника", err)
	}
	if _, err := g.NewAccountingID(); !errors.Is(err, errNoEntropy) {
		t.Fatalf("NewAccountingID() = %v, ожидался проброс ошибки источника", err)
	}

	secret, err := g.NewClientUUID()
	if !errors.Is(err, errNoEntropy) {
		t.Fatalf("NewClientUUID() = %v, ожидался проброс ошибки источника", err)
	}
	if !secret.IsZero() {
		t.Fatal("при ошибке возвращён непустой client_uuid")
	}
}

// Генератор берёт энтропию из подставленного источника, а не из глобального
// состояния: иначе тест выше ничего бы не проверял.
func TestGeneratorUsesInjectedRandom(t *testing.T) {
	source := bytes.Repeat([]byte{0xab}, 64)
	g := newGeneratorWithRandom(bytes.NewReader(source))

	id, err := g.NewAccountingID()
	if err != nil {
		t.Fatalf("NewAccountingID() ошибка: %v", err)
	}

	want := AccountingIDPrefix + strings.Repeat(string(accountingIDAlphabet[0xab&accountingIDMask]), accountingIDBodyLen)
	if id != want {
		t.Fatalf("accounting_id = %q, ожидалось %q", id, want)
	}
}
