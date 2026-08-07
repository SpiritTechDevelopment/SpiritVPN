package crypto

import (
	"bytes"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func testKey(t *testing.T, id string, fill byte) Key {
	t.Helper()

	secret := bytes.Repeat([]byte{fill}, KeySize)
	key, err := NewKey(id, secret)
	if err != nil {
		t.Fatalf("NewKey() ошибка: %v", err)
	}
	return key
}

func testCipher(t *testing.T, id string, fill byte) *Cipher {
	t.Helper()

	c, err := NewCipher(testKey(t, id, fill))
	if err != nil {
		t.Fatalf("NewCipher() ошибка: %v", err)
	}
	return c
}

func TestCipherRoundTrip(t *testing.T) {
	c := testCipher(t, "dev-1", 0x01)
	secret := NewClientUUID(secretUUID)

	sealed, err := c.Seal(secret)
	if err != nil {
		t.Fatalf("Seal() ошибка: %v", err)
	}

	if sealed.KeyID != "dev-1" {
		t.Fatalf("KeyID = %q, ожидался dev-1", sealed.KeyID)
	}
	if len(sealed.Blob) != SealedBlobSize {
		t.Fatalf("длина блоба = %d, ожидалось %d", len(sealed.Blob), SealedBlobSize)
	}
	if bytes.Contains(sealed.Blob, secretUUID[:]) {
		t.Fatal("открытый client_uuid присутствует в блобе")
	}
	if sealed.IsZero() {
		t.Fatal("IsZero() = true у заполненного credential")
	}

	opened, err := c.Open(sealed)
	if err != nil {
		t.Fatalf("Open() ошибка: %v", err)
	}
	if opened.Reveal() != secretUUID {
		t.Fatalf("Open() вернул %v, ожидалось исходное значение", opened.Reveal())
	}
}

// Два Seal одного значения обязаны различаться: одинаковые блобы выдали бы, что у
// двух access один и тот же client_uuid.
func TestCipherSealIsRandomized(t *testing.T) {
	c := testCipher(t, "dev-1", 0x01)
	secret := NewClientUUID(secretUUID)

	first, err := c.Seal(secret)
	if err != nil {
		t.Fatalf("Seal() ошибка: %v", err)
	}
	second, err := c.Seal(secret)
	if err != nil {
		t.Fatalf("Seal() ошибка: %v", err)
	}

	if bytes.Equal(first.Blob, second.Blob) {
		t.Fatal("два Seal одного значения дали одинаковый блоб")
	}
	if bytes.Equal(first.Blob[:nonceSize], second.Blob[:nonceSize]) {
		t.Fatal("nonce повторился")
	}
}

func TestCipherOpenRejectsTamperedBlob(t *testing.T) {
	c := testCipher(t, "dev-1", 0x01)

	sealed, err := c.Seal(NewClientUUID(secretUUID))
	if err != nil {
		t.Fatalf("Seal() ошибка: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(SealedCredential) SealedCredential
	}{
		{"порча nonce", func(s SealedCredential) SealedCredential {
			blob := bytes.Clone(s.Blob)
			blob[0] ^= 0xff
			return SealedCredential{KeyID: s.KeyID, Blob: blob}
		}},
		{"порча ciphertext", func(s SealedCredential) SealedCredential {
			blob := bytes.Clone(s.Blob)
			blob[nonceSize] ^= 0xff
			return SealedCredential{KeyID: s.KeyID, Blob: blob}
		}},
		{"порча tag", func(s SealedCredential) SealedCredential {
			blob := bytes.Clone(s.Blob)
			blob[len(blob)-1] ^= 0xff
			return SealedCredential{KeyID: s.KeyID, Blob: blob}
		}},
		{"обрезанный блоб", func(s SealedCredential) SealedCredential {
			return SealedCredential{KeyID: s.KeyID, Blob: s.Blob[:len(s.Blob)-1]}
		}},
		{"пустой блоб", func(s SealedCredential) SealedCredential {
			return SealedCredential{KeyID: s.KeyID}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.Open(tc.mutate(sealed)); !errors.Is(err, ErrCiphertextInvalid) {
				t.Fatalf("ошибка = %v, ожидалась ErrCiphertextInvalid", err)
			}
		})
	}
}

// Чужой ключ под тем же key_id не должен расшифровывать записи: key_id входит в
// AAD, но сам по себе доступом не является.
func TestCipherOpenRejectsForeignKey(t *testing.T) {
	origin := testCipher(t, "dev-1", 0x01)
	foreign := testCipher(t, "dev-1", 0x02)

	sealed, err := origin.Seal(NewClientUUID(secretUUID))
	if err != nil {
		t.Fatalf("Seal() ошибка: %v", err)
	}

	if _, err := foreign.Open(sealed); !errors.Is(err, ErrCiphertextInvalid) {
		t.Fatalf("ошибка = %v, ожидалась ErrCiphertextInvalid", err)
	}
}

// В v1 active key ровно один, decrypt-only ключей нет (§7): чужой key_id — это
// подмена конфигурации, и её нужно отличать от повреждённых данных.
func TestCipherOpenRejectsUnknownKeyID(t *testing.T) {
	c := testCipher(t, "dev-1", 0x01)

	sealed, err := c.Seal(NewClientUUID(secretUUID))
	if err != nil {
		t.Fatalf("Seal() ошибка: %v", err)
	}
	sealed.KeyID = "dev-2"

	if _, err := c.Open(sealed); !errors.Is(err, ErrUnknownKeyID) {
		t.Fatalf("ошибка = %v, ожидалась ErrUnknownKeyID", err)
	}
}

// key_id действительно входит в AAD, а не только сравнивается: блоб, выпущенный
// под другим key_id тем же секретом, не аутентифицируется.
func TestCipherKeyIDIsAuthenticated(t *testing.T) {
	origin := testCipher(t, "dev-2", 0x01)
	other := testCipher(t, "dev-1", 0x01)

	sealed, err := origin.Seal(NewClientUUID(secretUUID))
	if err != nil {
		t.Fatalf("Seal() ошибка: %v", err)
	}

	// Подменяем только идентификатор, секрет тот же.
	sealed.KeyID = other.KeyID()

	if _, err := other.Open(sealed); !errors.Is(err, ErrCiphertextInvalid) {
		t.Fatalf("ошибка = %v, ожидалась ErrCiphertextInvalid", err)
	}
}

// Ошибка расшифрования уходит в логи и метрики (§15), поэтому не должна нести ни
// блоба, ни секрета.
func TestCipherOpenErrorHasNoSecret(t *testing.T) {
	c := testCipher(t, "dev-1", 0x01)

	sealed, err := c.Seal(NewClientUUID(secretUUID))
	if err != nil {
		t.Fatalf("Seal() ошибка: %v", err)
	}
	blob := bytes.Clone(sealed.Blob)
	blob[nonceSize] ^= 0xff

	_, err = c.Open(SealedCredential{KeyID: sealed.KeyID, Blob: blob})
	if err == nil {
		t.Fatal("Open() принял испорченный блоб")
	}

	message := err.Error()
	if strings.Contains(message, secretUUID.String()) ||
		strings.Contains(strings.ToLower(message), "3f2504e0") {
		t.Fatalf("ошибка %q содержит client_uuid", message)
	}
}

func TestCipherSelfTest(t *testing.T) {
	if err := testCipher(t, "dev-1", 0x01).SelfTest(); err != nil {
		t.Fatalf("SelfTest() ошибка: %v", err)
	}
}

// Отказ CSPRNG на nonce — ошибка команды, а не молчаливое шифрование с нулевым
// nonce.
func TestCipherSealPropagatesRandomError(t *testing.T) {
	c, err := newCipherWithRandom(testKey(t, "dev-1", 0x01), failingReader{})
	if err != nil {
		t.Fatalf("newCipherWithRandom() ошибка: %v", err)
	}

	if _, err := c.Seal(NewClientUUID(secretUUID)); !errors.Is(err, errNoEntropy) {
		t.Fatalf("ошибка = %v, ожидался проброс ошибки источника", err)
	}
	if err := c.SelfTest(); !errors.Is(err, errNoEntropy) {
		t.Fatalf("SelfTest() = %v, ожидался проброс ошибки источника", err)
	}
}

func TestNewCipherRejectsEmptyKey(t *testing.T) {
	if _, err := NewCipher(Key{}); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("ошибка = %v, ожидалась ErrKeyMissing", err)
	}
}

// Размер блоба фиксирован и не зависит от значения: длина сама по себе ничего не
// выдаёт.
func TestSealedBlobSizeIsConstant(t *testing.T) {
	c := testCipher(t, "dev-1", 0x01)

	for i := 0; i < 32; i++ {
		value, err := uuid.NewRandomFromReader(rand.Reader)
		if err != nil {
			t.Fatalf("uuid.NewRandomFromReader() ошибка: %v", err)
		}
		sealed, err := c.Seal(NewClientUUID(value))
		if err != nil {
			t.Fatalf("Seal() ошибка: %v", err)
		}
		if len(sealed.Blob) != SealedBlobSize {
			t.Fatalf("длина блоба = %d, ожидалось %d", len(sealed.Blob), SealedBlobSize)
		}
	}
}
