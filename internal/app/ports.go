// Package app содержит use case'ы backend и порты, через которые они дотягиваются
// до инфраструктуры.
//
// Домен (internal/domain) детерминирован: он не читает часы, не генерирует
// идентификаторы и не ходит в БД. Всё это — работа use case, а конкретные
// реализации живут в пакетах-адаптерах (crypto, postgres, grpcsvc) и
// подставляются при сборке процесса.
package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	"github.com/RomanRyabinkin/SpiritVPN/internal/nodeagent"
)

// Clock — часы процесса.
//
// ВНИМАНИЕ: отсюда нельзя брать время, которое потом сравнивается со временем
// PostgreSQL. Единственный источник времени для доменных решений — now() базы,
// то есть момент начала транзакции: истечение entitlement, closed_at/started_at
// периодов и next_attempt_at операций считаются в SQL (решение 2, §11.1).
// Расхождение часов приложения и базы иначе означало бы, что дельта трафика не
// попадёт ни в один период.
//
// Легальные потребители — таймауты, backoff, интервалы worker'ов и метрики,
// то есть всё, что живёт внутри процесса и ни с чем в базе не сверяется.
type Clock interface {
	Now() time.Time
}

// IDs — генератор идентификаторов и credentials для новых access.
//
// Все методы возвращают ошибку: отказ CSPRNG обязан провалить команду, а не
// процесс. accounting_id и client_uuid глобально уникальны по построению, их
// уникальность подтверждается unique-индексами; коллизия — повод упасть, а не
// ретраить (решение 4).
type IDs interface {
	NewAccessID() (uuid.UUID, error)
	NewQuotaPeriodID() (uuid.UUID, error)
	NewOperationID() (uuid.UUID, error)
	NewAccountingID() (string, error)
	NewClientUUID() (crypto.ClientUUID, error)
}

// AgentDispatcher — исходящая сетевая поверхность backend (§9).
//
// Типы приходят из пакета nodeagent, а не дублируются здесь (решение 45):
// Endpoint, User и Outcome — плоские данные без инфраструктуры внутри, и
// *nodeagent.Client удовлетворяет этому порту без единой строки переходника.
// Прецедент тот же, по которому границу порта уже пересекают crypto.ClientUUID и
// crypto.SealedCredential; своя копия трёх структур означала бы синхронизацию их
// полей руками при каждом изменении контракта агента.
//
// Ошибок методы не возвращают: любой отказ уже классифицирован в Outcome, и
// диспетчеру всегда есть что записать в agent_operations.
type AgentDispatcher interface {
	EnsureUserPresent(ctx context.Context, endpoint nodeagent.Endpoint, operationID string, user nodeagent.User) nodeagent.Outcome
	EnsureUserAbsent(ctx context.Context, endpoint nodeagent.Endpoint, operationID, accountingID string) nodeagent.Outcome
}

// ReconcileAgent — то, что authoritative reconcile требует от агента (§10).
//
// Порт отдельный от AgentDispatcher по той же причине, по которой отдельным
// сделан UsageAgent: у полного набора нет строки в agent_operations, свой
// deadline (он на два порядка крупнее одного Ensure) и свой ответ — агент
// возвращает сводку изменений, а не только исход.
type ReconcileAgent interface {
	ReconcileUsers(
		ctx context.Context,
		endpoint nodeagent.Endpoint,
		operationID string,
		users []nodeagent.User,
	) nodeagent.ReconcileResult
}

// Jitter — источник случайности для backoff (§9).
//
// Отдельный порт, а не math/rand напрямую: домен случайности не производит
// (см. domain.BackoffDelay), а тест задержки повтора должен быть детерминирован.
type Jitter interface {
	// Unit возвращает значение в [0,1).
	Unit() float64
}

// CredentialSealer — application-level шифрование client_uuid (§7).
//
// Seal вызывается при создании access, Open — только там, где открытое значение
// действительно нужно (в v1 это построение VLESS URI на время ответа, §8).
type CredentialSealer interface {
	Seal(crypto.ClientUUID) (crypto.SealedCredential, error)
	Open(crypto.SealedCredential) (crypto.ClientUUID, error)

	// KeyID — идентификатор active key для колонки encryption_key_id.
	KeyID() string
}
