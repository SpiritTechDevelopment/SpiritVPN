// Package domain содержит чистые правила customer access из
// BACKEND_DOMAIN_AGREEMENTS.md §4 и §5.
//
// Пакет детерминирован: он не читает часы, не генерирует идентификаторы и не
// ходит в БД. Всё недетерминированное — текущее время, access_id, accounting_id
// и client_uuid — приходит параметрами от вызывающего слоя. Поэтому каждый тест
// здесь — таблица «вход → ожидаемый план», без моков и подмены времени.
//
// Время приходит из PostgreSQL (now() = момент начала транзакции), а не из часов
// процесса: истечение сверяется со временем БД (§11.1), а при renewal closed_at
// старого периода обязан совпасть со started_at нового с точностью до значения,
// иначе дельта трафика не попадёт ни в один период (§11, §12).
package domain

import (
	"time"

	"github.com/google/uuid"
)

// NodeID — стабильная инфраструктурная идентичность ноды (§3). IP, домен и
// endpoint агента идентичностью не являются и могут меняться.
type NodeID string

// LogicalTargetKey — логическая цель access, стабильная поверх поколений (§4):
// для FREEDOM это node_id его ноды, для BRIDGE — routing_key связи.
type LogicalTargetKey string

// AccessKind — вид access. FREEDOM выпускается на каждую ноду fleet, BRIDGE — на
// каждую bridge relation (§4).
type AccessKind string

const (
	AccessKindFreedom AccessKind = "FREEDOM"
	AccessKindBridge  AccessKind = "BRIDGE"
)

// DesiredState — желаемое присутствие юзера на входной ноде (§9).
type DesiredState string

const (
	DesiredStatePresent DesiredState = "PRESENT"
	DesiredStateAbsent  DesiredState = "ABSENT"
)

// FreedomEgressKey — зарезервированное значение egress_key для FREEDOM: пустая
// строка означает локальный выход самой ноды (direct) (§6, §7).
const FreedomEgressKey = ""

// BridgeRoute — направленная BRIDGE-связь внутри fleet (§6).
type BridgeRoute struct {
	// RoutingKey однозначно идентифицирует связь внутри fleet; отдельного
	// bridge_id нет (§3).
	RoutingKey string
	// EntryNodeID — нода, на которую ставится client_uuid customer. На EXIT
	// customer credential не устанавливается (§4).
	EntryNodeID NodeID
	ExitNodeID  NodeID
	// EgressTag — тег exit-outbound на входной ноде. Передаётся агенту дословно
	// как User.egress_key; backend ничего из него не деривит (§6, §7).
	EgressTag string
}

// FleetTopology — текущая проекция топологии одного fleet из manifest (§6).
type FleetTopology struct {
	FleetID int64
	Nodes   []NodeID
	Bridges []BridgeRoute
}

// Access — индивидуальный доступ customer к одной логической цели (§4).
//
// Структура намеренно уже строки vpn_accesses: правила §4/§5 не читают
// encrypted_client_uuid (он непрозрачен и у существующего access неизменен) и не
// читают apply_state — тот является результатом доставки, а не желаемым
// состоянием, и версию desired не двигает (решение 3.1).
type Access struct {
	ID               uuid.UUID
	Kind             AccessKind
	LogicalTargetKey LogicalTargetKey
	Generation       int32
	EntryNodeID      NodeID
	EgressKey        string
	AccountingID     string
	DesiredState     DesiredState
	DesiredVersion   int64
	// Retired соответствует retired_at IS NOT NULL. Ретайрнутые access не
	// выдаются и не восстанавливаются, но участвуют в расчёте поколения при
	// повторном появлении той же цели (§4).
	Retired bool
}

// Entitlement — корневая строка customer, точка сериализации всех изменений его
// состояния (§11.1).
type Entitlement struct {
	FleetID   int64
	ExpiresAt time.Time
	// LastCommandNumber — гейт порядка команд product (§5, правила 1–3).
	LastCommandNumber uint64
	// DesiredVersion — внутренний счётчик фактических изменений desired state
	// customer; спекой не используется (решение 3.5).
	DesiredVersion int64
}

// QuotaPeriod — период учёта квоты. Текущий период — единственный с
// closed_at IS NULL (§11).
type QuotaPeriod struct {
	ID              uuid.UUID
	UsageQuotaBytes uint64
	StartedAt       time.Time
}

// NodeQuotaUsage — расход трафика customer на одной ноде внутри периода.
// Квота применяется независимо на каждой ноде: превышение на одной не влияет на
// доступ на других (§4).
type NodeQuotaUsage struct {
	NodeID     NodeID
	TotalBytes uint64
	// ExhaustedAt одновременно факт исчерпания квоты и причина node-level
	// блокировки; отдельной таблицы блокировок нет (§11).
	ExhaustedAt *time.Time
}

// ApplyCommand — нормализованный запрос ApplyCustomerAccess (§5).
// ExpiresAt — абсолютный момент окончания, а не длительность.
type ApplyCommand struct {
	CustomerID      string
	FleetID         int64
	UsageQuotaBytes uint64
	ExpiresAt       time.Time
	CommandNumber   uint64
}

// maxCustomerIDBytes — верхняя граница непрозрачного customer_id (§3).
const maxCustomerIDBytes = 256
