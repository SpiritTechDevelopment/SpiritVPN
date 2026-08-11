package metrics

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
)

// fakeSealer — шифр с заданными отказами.
type fakeSealer struct {
	sealErr error
	openErr error
}

func (f *fakeSealer) Seal(crypto.ClientUUID) (crypto.SealedCredential, error) {
	return crypto.SealedCredential{}, f.sealErr
}

func (f *fakeSealer) Open(crypto.SealedCredential) (crypto.ClientUUID, error) {
	return crypto.ClientUUID{}, f.openErr
}

func (f *fakeSealer) KeyID() string { return "key-1" }

func TestSealerCountsOpenFailures(t *testing.T) {
	registry := New()
	sealer := registry.WrapSealer(&fakeSealer{openErr: errors.New("ключ не подходит")})

	if _, err := sealer.Open(crypto.SealedCredential{}); err == nil {
		t.Fatal("ошибка расшифрования не проброшена наружу")
	}

	if got := testutil.ToFloat64(registry.credentialOpenErrors); got != 1 {
		t.Errorf("ошибок расшифрования %v, ожидалась 1", got)
	}
}

func TestSealerIgnoresSuccessAndSeal(t *testing.T) {
	registry := New()
	sealer := registry.WrapSealer(&fakeSealer{sealErr: errors.New("шифрование сломано")})

	if _, err := sealer.Open(crypto.SealedCredential{}); err != nil {
		t.Fatalf("успешное расшифрование вернуло ошибку: %v", err)
	}
	// Отказ Seal валит команду целиком и виден как ошибка RPC; §15 просит именно
	// ошибки расшифрования, которые деградируют молча.
	if _, err := sealer.Seal(crypto.ClientUUID{}); err == nil {
		t.Fatal("ошибка шифрования не проброшена наружу")
	}

	if got := testutil.ToFloat64(registry.credentialOpenErrors); got != 0 {
		t.Errorf("ошибок расшифрования %v, ожидался 0", got)
	}
}

// TestSealerPassesKeyIDThrough — KeyID уезжает в колонку encryption_key_id, и
// подмена его декоратором сделала бы credential нерасшифровываемым.
func TestSealerPassesKeyIDThrough(t *testing.T) {
	registry := New()

	if got := registry.WrapSealer(&fakeSealer{}).KeyID(); got != "key-1" {
		t.Errorf("KeyID %q, ожидался %q", got, "key-1")
	}
}
