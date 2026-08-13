// Package crypto содержит credential-значения customer access, их генерацию и
// application-level шифрование.
//
// Пакет листовой: он не ходит в БД и в сеть, единственное обращение к файловой
// системе — чтение ключа при старте. Поэтому он подставляется в порты app как
// адаптер, но зависимостей на другие пакеты проекта не имеет.
package crypto

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/google/uuid"
)

// redactedClientUUID — единственное строковое представление секрета.
const redactedClientUUID = "client_uuid(redacted)"

// ClientUUID — секретный VLESS UUID клиентского access.
//
// Тип существует ради одной гарантии: значение не может попасть в лог, метрику,
// трейс или audit metadata случайно. Все стандартные пути превращения значения в
// текст — fmt, encoding/json, log/slog — редактированы, открытое значение отдаёт
// только явный Reveal. Единственный его легальный потребитель в v1 — построитель
// VLESS URI, который расшифровывает credential на время ответа.
//
// Гарантия не абсолютна: fmt не умеет вызывать методы у неэкспортированных полей
// чужой структуры и напечатает такое поле как сырой [16]byte. Значит, ClientUUID
// нельзя класть в неэкспортированное поле структуры, которую где-то печатают
// целиком.
type ClientUUID struct {
	value uuid.UUID
}

// NewClientUUID оборачивает готовое значение. Нужен на пути расшифрования и в
// тестах; новые credentials выдаёт Generator.
func NewClientUUID(value uuid.UUID) ClientUUID {
	return ClientUUID{value: value}
}

// Reveal отдаёт открытое значение. Единственная точка, где секрет покидает тип, —
// каждый её вызов должен быть осознанным.
func (c ClientUUID) Reveal() uuid.UUID {
	return c.value
}

// IsZero сообщает, что значение не заполнено.
func (c ClientUUID) IsZero() bool {
	return c.value == uuid.Nil
}

func (c ClientUUID) String() string {
	return redactedClientUUID
}

// GoString реализует fmt.GoStringer. У fmt приоритет выше у Formatter, поэтому
// %#v закрывает Format; метод остаётся для прямых вызовов и для случая, если
// Format когда-нибудь уберут.
func (c ClientUUID) GoString() string {
	return redactedClientUUID
}

// Format перехватывает у fmt все глаголы разом.
//
// String закрывает только v, s, q, x и X; на числовых глаголах (%d, %b, %o, %U,
// %c) fmt печатает значение сам и выдал бы байты uuid в обход редакции.
func (c ClientUUID) Format(f fmt.State, verb rune) {
	if verb == 'q' {
		fmt.Fprintf(f, "%q", redactedClientUUID)
		return
	}
	_, _ = io.WriteString(f, redactedClientUUID)
}

// MarshalText закрывает encoding/json, encoding/xml и все кодировки поверх
// TextMarshaler.
func (c ClientUUID) MarshalText() ([]byte, error) {
	return []byte(redactedClientUUID), nil
}

// MarshalJSON редактирует значение, а не возвращает ошибку: сериализация обычно
// применяется к логам и диагностике, и падение всего объекта там хуже, чем
// заглушка вместо одного поля.
func (c ClientUUID) MarshalJSON() ([]byte, error) {
	return []byte(`"` + redactedClientUUID + `"`), nil
}

// LogValue закрывает log/slog.
func (c ClientUUID) LogValue() slog.Value {
	return slog.StringValue(redactedClientUUID)
}
