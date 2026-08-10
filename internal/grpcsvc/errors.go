package grpcsvc

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
)

// msgInternal — единственное, что уходит наружу при неопознанной ошибке.
const msgInternal = "внутренняя ошибка"

// Стабильные коды ошибок для structured logs (§15).
//
// Они намеренно отделены от gRPC-кода и от текста сообщения: gRPC-код слишком
// груб (три доменных исхода делят INVALID_ARGUMENT), а текст предназначен
// человеку и может меняться. По этим кодам строятся alert'ы, поэтому
// переименование любого из них — изменение контракта наблюдаемости, а не
// рефакторинг.
const (
	codeOK = "OK"

	codeInvalidCustomerID    = "INVALID_CUSTOMER_ID"
	codeInvalidFleetID       = "INVALID_FLEET_ID"
	codeInvalidQuota         = "INVALID_QUOTA"
	codeInvalidCommandNumber = "INVALID_COMMAND_NUMBER"
	codeExpiryNotInFuture    = "EXPIRY_NOT_IN_FUTURE"
	codeCustomerNotFound     = "CUSTOMER_NOT_FOUND"
	codeFleetNotFound        = "FLEET_NOT_FOUND"
	codeFleetMismatch        = "FLEET_MISMATCH"
	codeExpiryRegression     = "EXPIRY_REGRESSION"
	codeOpenPeriodMissing    = "OPEN_PERIOD_MISSING"

	codeManifestSchemaVersion      = "MANIFEST_SCHEMA_VERSION"
	codeManifestRevisionInvalid    = "MANIFEST_REVISION_INVALID"
	codeManifestTooLarge           = "MANIFEST_TOO_LARGE"
	codeManifestDuplicate          = "MANIFEST_DUPLICATE"
	codeManifestUnknownNode        = "MANIFEST_UNKNOWN_NODE"
	codeManifestNodeInvalid        = "MANIFEST_NODE_INVALID"
	codeManifestBridgeInvalid      = "MANIFEST_BRIDGE_INVALID"
	codeManifestRevisionRegression = "MANIFEST_REVISION_REGRESSION"
	codeManifestDigestConflict     = "MANIFEST_DIGEST_CONFLICT"
	codeManifestFleetMissing       = "MANIFEST_FLEET_MISSING"
	codeManifestDestructive        = "MANIFEST_DESTRUCTIVE"
	codeManifestBridgePair         = "MANIFEST_BRIDGE_PAIR_IMMUTABLE"

	codeCanceled         = "CANCELED"
	codeDeadlineExceeded = "DEADLINE_EXCEEDED"
	codeUnauthenticated  = "UNAUTHENTICATED"
	codePermissionDenied = "PERMISSION_DENIED"
	codeInternal         = "INTERNAL"
)

// statusError — то, что уходит клиенту, вместе со стабильным кодом для логов.
//
// gRPC сериализует ошибку через интерфейс GRPCStatus(), поэтому наружу уходит
// ровно status и ничего больше: стабильный код клиенту не виден. Его достаёт
// logging-interceptor через errors.As, и это единственная причина, по которой
// два значения едут вместе, а не разъезжаются по разным каналам.
type statusError struct {
	status *status.Status
	code   string
}

func (e *statusError) Error() string { return e.status.Message() }

// GRPCStatus — то, за что цепляется status.FromError на стороне gRPC.
func (e *statusError) GRPCStatus() *status.Status { return e.status }

// newStatusError собирает ответ клиенту и запись для лога одним значением.
func newStatusError(c codes.Code, stable, message string) error {
	return &statusError{status: status.New(c, message), code: stable}
}

// stableCode достаёт код для лога. Ошибка, не прошедшая через newStatusError,
// считается внутренней: неизвестное происхождение — это и есть INTERNAL.
func stableCode(err error) string {
	if err == nil {
		return codeOK
	}

	var se *statusError
	if errors.As(err, &se) {
		return se.code
	}
	return codeInternal
}

// errorMapping — доменные исходы, их коды и сообщения (§5, шапка domain/errors.go).
//
// Сообщения зафиксированы здесь, а не берутся из err.Error(), намеренно. Use
// case оборачивает инфраструктурные ошибки (fmt.Errorf("блокировка entitlement:
// %w", err)), и текст обёртки вынес бы наружу детали драйвера PostgreSQL: имена
// таблиц и колонок, параметры подключения, куски схемы. Наружу уходит только то,
// что перечислено в этой таблице.
//
// Порядок строк значения не имеет: сентинелы взаимно исключающие.
var errorMapping = []struct {
	sentinel error
	code     codes.Code
	stable   string
	message  string
}{
	{domain.ErrCustomerIDInvalid, codes.InvalidArgument, codeInvalidCustomerID, "customer_id должен быть непустым и не длиннее 256 байт"},
	{domain.ErrFleetIDInvalid, codes.InvalidArgument, codeInvalidFleetID, "vpn_fleet_id должен быть > 0"},
	{domain.ErrQuotaInvalid, codes.InvalidArgument, codeInvalidQuota, "usage_quota_bytes должен быть > 0"},
	{domain.ErrCommandNumberInvalid, codes.InvalidArgument, codeInvalidCommandNumber, "command_number должен быть > 0"},
	{domain.ErrExpiryNotInFuture, codes.InvalidArgument, codeExpiryNotInFuture, "expires_at должен быть в будущем"},
	{domain.ErrCustomerNotFound, codes.NotFound, codeCustomerNotFound, "customer не найден"},
	{domain.ErrFleetNotFound, codes.NotFound, codeFleetNotFound, "fleet не найден"},
	{domain.ErrFleetMismatch, codes.FailedPrecondition, codeFleetMismatch, "customer уже привязан к другому fleet"},
	{domain.ErrExpiryRegression, codes.FailedPrecondition, codeExpiryRegression, "сокращение expires_at не поддерживается"},

	// Нарушение инварианта §11 (ровно один период с closed_at IS NULL), а не
	// ошибка вызывающего: наружу уходит обезличенное сообщение, но в логе исход
	// отличим от прочих INTERNAL.
	{domain.ErrOpenPeriodMissing, codes.Internal, codeOpenPeriodMissing, msgInternal},
}

// manifestErrorMapping — правила §6 и их коды.
//
// Сообщение здесь не задаётся: в отличие от таблицы выше, наружу уходит текст
// самой ошибки вместе с деталью (какая нода, какая связь, какое значение).
// Так можно ровно потому, что domain.ManifestValidationError рождается только в
// чистых функциях над payload вызывающего: ничего, кроме значений из его же
// запроса, в нём нет, а обёрнутая ошибка драйвера этим типом не является и сюда
// не попадёт. Вызывающий — infrastructure CI/CD (§14), и деталь экономит ему
// разбор целого снапшота вручную.
//
// Коды разведены по тому, от чего зависит исход: форма снапшота —
// INVALID_ARGUMENT, конфликт с уже принятым состоянием — FAILED_PRECONDITION.
var manifestErrorMapping = []struct {
	sentinel error
	code     codes.Code
	stable   string
}{
	{domain.ErrManifestSchemaVersion, codes.InvalidArgument, codeManifestSchemaVersion},
	{domain.ErrManifestRevisionInvalid, codes.InvalidArgument, codeManifestRevisionInvalid},
	{domain.ErrManifestTooLarge, codes.InvalidArgument, codeManifestTooLarge},
	{domain.ErrManifestDuplicate, codes.InvalidArgument, codeManifestDuplicate},
	{domain.ErrManifestUnknownNode, codes.InvalidArgument, codeManifestUnknownNode},
	{domain.ErrManifestNodeInvalid, codes.InvalidArgument, codeManifestNodeInvalid},
	{domain.ErrManifestBridgeInvalid, codes.InvalidArgument, codeManifestBridgeInvalid},

	{domain.ErrManifestRevisionRegression, codes.FailedPrecondition, codeManifestRevisionRegression},
	{domain.ErrManifestDigestConflict, codes.FailedPrecondition, codeManifestDigestConflict},
	{domain.ErrManifestFleetMissing, codes.FailedPrecondition, codeManifestFleetMissing},
	{domain.ErrManifestDestructive, codes.FailedPrecondition, codeManifestDestructive},
	{domain.ErrManifestBridgePairImmutable, codes.FailedPrecondition, codeManifestBridgePair},
}

// statusFromManifestError отображает нарушение правил §6. Возвращает nil, если
// ошибка к манифесту не относится.
func statusFromManifestError(err error) error {
	var validation *domain.ManifestValidationError
	if !errors.As(err, &validation) {
		return nil
	}

	for _, mapping := range manifestErrorMapping {
		if errors.Is(validation.Rule, mapping.sentinel) {
			return newStatusError(mapping.code, mapping.stable, validation.Error())
		}
	}

	// Сентинел есть, а строки в таблице нет: правило добавили, а код забыли.
	// Наружу уходит обезличенное сообщение, но в логе исход не сливается с
	// прочими INTERNAL.
	return newStatusError(codes.Internal, codeInternal, msgInternal)
}

// statusFromError отображает ошибку use case в gRPC-статус.
//
// Порядок проверок существенен:
//
//  1. доменные сентинелы — они точные и имеют приоритет. Иначе отмена вызова,
//     случившаяся сразу после доменной ошибки, подменила бы FAILED_PRECONDITION
//     на CANCELED и потеряла бы разметку исхода;
//  2. входящий контекст. Отмена и истёкший дедлайн определяются по нему, а НЕ по
//     тексту ошибки (решение 12): одна и та же context.Canceled означает разное в
//     зависимости от того, чей контекст закрылся. Закрытый ctx хендлера — причина
//     на стороне вызывающего. Живой ctx при context.Canceled изнутри — дефект
//     сервера, и он обязан остаться INTERNAL, иначе будущий внутренний таймаут на
//     транзакцию замаскируется под отмену клиентом;
//  3. INTERNAL по умолчанию (§5: всё неопознанное).
//
// Шаг 2 — не отступление от §5, а заполнение пробела: таблица §5 перечисляет
// доменные исходы и обрыв стрима не рассматривает. Смысл шага в наблюдаемости
// §15: штатный рестарт вызывающего с полусотней вызовов в полёте не должен
// выглядеть в метриках как полсотни отказов backend.
func statusFromError(ctx context.Context, err error) error {
	// Правила §6 идут первыми и по тем же соображениям, что и доменные сентинелы
	// ниже: исход точный, и отмена вызова не должна его подменять.
	if manifestErr := statusFromManifestError(err); manifestErr != nil {
		return manifestErr
	}

	for _, mapping := range errorMapping {
		if errors.Is(err, mapping.sentinel) {
			return newStatusError(mapping.code, mapping.stable, mapping.message)
		}
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		switch {
		case errors.Is(ctxErr, context.Canceled):
			return newStatusError(codes.Canceled, codeCanceled, "запрос отменён вызывающим")
		case errors.Is(ctxErr, context.DeadlineExceeded):
			return newStatusError(codes.DeadlineExceeded, codeDeadlineExceeded, "истёк дедлайн запроса")
		}
	}

	return newStatusError(codes.Internal, codeInternal, msgInternal)
}
