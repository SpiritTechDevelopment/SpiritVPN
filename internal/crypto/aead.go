package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
)

const (
	// nonceSize — стандартный размер nonce для GCM.
	nonceSize = 12

	// SealedBlobSize — nonce + ciphertext + tag для 16-байтного uuid.
	// Размер фиксирован, поэтому длина блоба сама по себе ничего не выдаёт.
	SealedBlobSize = nonceSize + len(uuid.UUID{}) + 16
)

var (
	// ErrUnknownKeyID — запись зашифрована ключом, которого у процесса нет. В v1
	// active key ровно один, decrypt-only ключей нет (§7), поэтому такое означает
	// подмену конфигурации, а не штатную ротацию.
	ErrUnknownKeyID = errors.New("crypto: запись зашифрована неизвестным ключом")

	// ErrCiphertextInvalid — блоб не расшифровывается: повреждён, подделан либо
	// зашифрован другим ключом. Отдельный сентинел нужен под метрику «ошибки
	// расшифрования» (§15).
	ErrCiphertextInvalid = errors.New("crypto: client_uuid не расшифровывается")
)

// SealedCredential — зашифрованный client_uuid в форме, которая ложится на
// колонки vpn_accesses (§11): KeyID → encryption_key_id, Blob →
// encrypted_client_uuid.
//
// Спека описывает формат как `key_id + nonce + ciphertext` (§7); схема хранит
// key_id отдельной колонкой, поэтому в блобе остаётся `nonce || ciphertext||tag`,
// а key_id дополнительно уходит в AAD и тем самым остаётся частью
// аутентифицируемого сообщения.
type SealedCredential struct {
	KeyID string
	Blob  []byte
}

// IsZero сообщает, что credential не заполнен.
func (s SealedCredential) IsZero() bool {
	return s.KeyID == "" && len(s.Blob) == 0
}

// Cipher шифрует и расшифровывает client_uuid на одном active key (§7).
//
// Алгоритм — AES-256-GCM из stdlib. Nonce случайный на каждый Seal: при 96-битном
// nonce и порядке 10^5–10^6 записей на ключ вероятность повтора пренебрежима, а
// счётчик потребовал бы синхронизированного состояния между репликами.
type Cipher struct {
	keyID  string
	aead   cipher.AEAD
	random io.Reader
}

// NewCipher собирает шифратор поверх active key.
func NewCipher(key Key) (*Cipher, error) {
	if key.IsZero() {
		return nil, ErrKeyMissing
	}

	block, err := aes.NewCipher(key.secret)
	if err != nil {
		return nil, fmt.Errorf("crypto: инициализация AES: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: инициализация GCM: %w", err)
	}
	if aead.NonceSize() != nonceSize {
		return nil, fmt.Errorf("crypto: неожиданный размер nonce %d", aead.NonceSize())
	}

	return &Cipher{keyID: key.ID(), aead: aead}, nil
}

// newCipherWithRandom — конструктор для тестов: подменяет источник nonce.
func newCipherWithRandom(key Key, random io.Reader) (*Cipher, error) {
	c, err := NewCipher(key)
	if err != nil {
		return nil, err
	}
	c.random = random
	return c, nil
}

// KeyID — идентификатор active key для колонки encryption_key_id.
func (c *Cipher) KeyID() string {
	return c.keyID
}

func (c *Cipher) reader() io.Reader {
	if c.random != nil {
		return c.random
	}
	return rand.Reader
}

// Seal шифрует client_uuid для хранения.
func (c *Cipher) Seal(value ClientUUID) (SealedCredential, error) {
	blob := make([]byte, nonceSize, SealedBlobSize)
	if _, err := io.ReadFull(c.reader(), blob); err != nil {
		return SealedCredential{}, fmt.Errorf("crypto: чтение CSPRNG для nonce: %w", err)
	}

	plaintext := value.Reveal()
	blob = c.aead.Seal(blob, blob[:nonceSize], plaintext[:], c.additionalData())

	return SealedCredential{KeyID: c.keyID, Blob: blob}, nil
}

// Open расшифровывает client_uuid. Вызывать только там, где открытое значение
// действительно нужно — в v1 это построение VLESS URI на время ответа (§8).
//
// Ошибка намеренно не содержит ни блоба, ни подробностей от GCM: она попадает в
// логи и метрики.
func (c *Cipher) Open(sealed SealedCredential) (ClientUUID, error) {
	if sealed.KeyID != c.keyID {
		return ClientUUID{}, fmt.Errorf("%w: %s", ErrUnknownKeyID, sealed.KeyID)
	}
	if len(sealed.Blob) != SealedBlobSize {
		return ClientUUID{}, fmt.Errorf("%w: длина блоба %d, ожидалось %d",
			ErrCiphertextInvalid, len(sealed.Blob), SealedBlobSize)
	}

	plaintext, err := c.aead.Open(nil, sealed.Blob[:nonceSize], sealed.Blob[nonceSize:], c.additionalData())
	if err != nil {
		return ClientUUID{}, ErrCiphertextInvalid
	}

	var value uuid.UUID
	copy(value[:], plaintext)

	return NewClientUUID(value), nil
}

// SelfTest проверяет, что active key рабочий: readiness требует валидный
// encryption key (§15). Проба фиктивная, к данным не обращается.
func (c *Cipher) SelfTest() error {
	probe := NewClientUUID(uuid.UUID{
		0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77,
		0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff,
	})

	sealed, err := c.Seal(probe)
	if err != nil {
		return fmt.Errorf("crypto: self-test шифрования: %w", err)
	}
	opened, err := c.Open(sealed)
	if err != nil {
		return fmt.Errorf("crypto: self-test расшифрования: %w", err)
	}
	if opened.Reveal() != probe.Reveal() {
		return errors.New("crypto: self-test вернул другое значение")
	}

	return nil
}

// additionalData связывает блоб с идентичностью ключа: даже если запись подсунуть
// под другой ключ с тем же key_id в колонке, аутентификация не пройдёт.
func (c *Cipher) additionalData() []byte {
	return []byte(c.keyID)
}
