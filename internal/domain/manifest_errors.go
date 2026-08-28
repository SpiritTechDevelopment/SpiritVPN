package domain

import (
	"errors"
	"fmt"
)

// Ошибки приёма манифеста. Сопоставление с gRPC-кодами живёт в grpcsvc и
// опирается на эти сентинелы через errors.Is.
//
// Разделены они по одному признаку — от чего зависит исход:
//
//	INVALID_ARGUMENT     — нарушения формы самого снапшота. Зависят только от
//	                       запроса, воспроизводятся без базы;
//	FAILED_PRECONDITION  — конфликты с уже принятым состоянием. Тот же запрос
//	                       завтра может стать валидным (или наоборот).
//
// Разделение введено здесь и совпадает с тем, как разводятся исходы командного
// пути.
var (
	// --- форма снапшота: INVALID_ARGUMENT ---

	// ErrManifestSchemaVersion — schema_version не входит в поддерживаемый набор.
	ErrManifestSchemaVersion = errors.New("domain: неподдерживаемая schema_version манифеста")

	// ErrManifestRevisionInvalid — revision не положительна.
	ErrManifestRevisionInvalid = errors.New("domain: revision манифеста должна быть > 0")

	// ErrManifestTooLarge — превышен один из лимитов размера манифеста.
	ErrManifestTooLarge = errors.New("domain: манифест превышает лимиты")

	// ErrManifestDuplicate — повторяющийся node_id, vpn_fleet_id или routing_key
	// внутри снапшота.
	ErrManifestDuplicate = errors.New("domain: дубликат идентификатора в манифесте")

	// ErrManifestUnknownNode — ссылка на node_id, которого нет в snapshot.nodes
	// (все node references обязаны существовать).
	ErrManifestUnknownNode = errors.New("domain: ссылка на неизвестную ноду")

	// ErrManifestNodeInvalid — обязательные поля agent или public не заполнены
	// либо содержат значение, не поддерживаемое v1.
	ErrManifestNodeInvalid = errors.New("domain: некорректные параметры ноды")

	// ErrManifestBridgeInvalid — связь нарушает правила: entry совпадает с
	// exit, одна из нод не входит в fleet, либо пуст egress_tag.
	ErrManifestBridgeInvalid = errors.New("domain: некорректная bridge-связь")

	// --- конфликты с принятым состоянием: FAILED_PRECONDITION ---

	// ErrManifestRevisionRegression — revision не больше последней принятой.
	// Повтор той же revision с тем же digest сюда не доходит: он идемпотентен.
	ErrManifestRevisionRegression = errors.New("domain: revision манифеста не возрастает")

	// ErrManifestDigestConflict — та же revision с другим каноническим digest.
	// Ровно то, ради чего digest и считается: две разные топологии под
	// одним номером означают рассогласованный источник, а не повтор доставки.
	ErrManifestDigestConflict = errors.New("domain: та же revision манифеста с другим digest")

	// ErrManifestFleetMissing — в снапшоте нет ранее принятого fleet.
	// Отклоняет весь манифест независимо от allow_destructive: принятый
	// vpn_fleet_id не удаляется и не переиспользуется.
	ErrManifestFleetMissing = errors.New("domain: ранее принятый fleet отсутствует в манифесте")

	// ErrManifestDestructive — снапшот удаляет ноду, membership или связь без
	// allow_destructive.
	ErrManifestDestructive = errors.New("domain: удаление требует allow_destructive")

	// ErrManifestBridgePairImmutable — у существующего routing_key изменилась
	// пара (entry_node_id, exit_node_id). Перенос route требует удаления
	// старого routing_key и добавления нового.
	ErrManifestBridgePairImmutable = errors.New("domain: пара entry/exit связи неизменяема")
)

// ManifestValidationError — нарушение правила манифеста вместе с деталью, которую
// безопасно показать вызывающему.
//
// Тип нужен ровно ради этой безопасности. Остальные доменные ошибки уходят
// наружу фиксированными сообщениями из таблицы grpcsvc: use case оборачивает в
// них ошибки драйвера PostgreSQL, и err.Error() вынес бы имена таблиц и
// параметры подключения. Здесь такого риска нет по построению — эти ошибки
// рождаются в чистых функциях над payload вызывающего и ничего, кроме значений
// из его же запроса, содержать не могут.
//
// Смысл — в диагностике: manifest-writer это infrastructure CI/CD, и
// «связь nl-1.to-de-1 ссылается на неизвестную ноду DE-9» экономит ему час
// против голого INVALID_ARGUMENT.
type ManifestValidationError struct {
	// Rule — сентинел из списка выше; по нему определяется gRPC-код.
	Rule error
	// Detail содержит только значения из запроса.
	Detail string
}

func (e *ManifestValidationError) Error() string {
	if e.Detail == "" {
		return e.Rule.Error()
	}
	return e.Rule.Error() + ": " + e.Detail
}

// Unwrap открывает сентинел для errors.Is.
func (e *ManifestValidationError) Unwrap() error { return e.Rule }

// manifestError собирает нарушение правила с деталью.
func manifestError(rule error, format string, args ...any) error {
	return &ManifestValidationError{Rule: rule, Detail: fmt.Sprintf(format, args...)}
}
