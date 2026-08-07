package crypto

import (
	"crypto/rand"
	"fmt"
	"io"

	"github.com/google/uuid"
)

// Generator выдаёт идентификаторы и credentials для новых access.
//
// Домен ничего не генерирует сам (он детерминирован), поэтому все значения
// приходят к нему параметрами от use case, а use case берёт их отсюда через порт
// app.IDs.
type Generator struct {
	// random — источник энтропии; nil означает crypto/rand.Reader.
	random io.Reader
}

// NewGenerator собирает генератор на системном CSPRNG.
func NewGenerator() *Generator {
	return &Generator{}
}

// newGeneratorWithRandom — конструктор для тестов: позволяет проверить и
// детерминированное отображение, и проброс ошибки источника.
func newGeneratorWithRandom(random io.Reader) *Generator {
	return &Generator{random: random}
}

func (g *Generator) reader() io.Reader {
	if g.random != nil {
		return g.random
	}
	return rand.Reader
}

// NewAccessID — первичный ключ vpn_accesses.
func (g *Generator) NewAccessID() (uuid.UUID, error) {
	return g.newUUID("access_id")
}

// NewQuotaPeriodID — внутренний идентификатор периода квоты; во внешний контракт
// не попадает (§5).
func (g *Generator) NewQuotaPeriodID() (uuid.UUID, error) {
	return g.newUUID("quota_period_id")
}

// NewAccountingID — см. NewAccountingID пакета.
func (g *Generator) NewAccountingID() (string, error) {
	return NewAccountingID(g.reader())
}

// NewClientUUID выдаёт секретный VLESS UUID клиентского access (§7).
//
// Используется uuid.NewRandom с явной ошибкой, а не uuid.New: последний паникует
// при отказе источника энтропии, и такой отказ обязан стать ошибкой команды, а не
// падением процесса.
func (g *Generator) NewClientUUID() (ClientUUID, error) {
	value, err := g.newUUID("client_uuid")
	if err != nil {
		return ClientUUID{}, err
	}
	return NewClientUUID(value), nil
}

func (g *Generator) newUUID(what string) (uuid.UUID, error) {
	value, err := uuid.NewRandomFromReader(g.reader())
	if err != nil {
		return uuid.Nil, fmt.Errorf("crypto: генерация %s: %w", what, err)
	}
	return value, nil
}
