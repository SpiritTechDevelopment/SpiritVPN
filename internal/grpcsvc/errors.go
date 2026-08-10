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

// errorMapping — доменные исходы и их коды (§5, шапка domain/errors.go).
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
	message  string
}{
	{domain.ErrCustomerIDInvalid, codes.InvalidArgument, "customer_id должен быть непустым и не длиннее 256 байт"},
	{domain.ErrFleetIDInvalid, codes.InvalidArgument, "vpn_fleet_id должен быть > 0"},
	{domain.ErrQuotaInvalid, codes.InvalidArgument, "usage_quota_bytes должен быть > 0"},
	{domain.ErrCommandNumberInvalid, codes.InvalidArgument, "command_number должен быть > 0"},
	{domain.ErrExpiryNotInFuture, codes.InvalidArgument, "expires_at должен быть в будущем"},
	{domain.ErrFleetNotFound, codes.NotFound, "fleet не найден"},
	{domain.ErrFleetMismatch, codes.FailedPrecondition, "customer уже привязан к другому fleet"},
	{domain.ErrExpiryRegression, codes.FailedPrecondition, "сокращение expires_at не поддерживается"},

	// Нарушение инварианта §11 (ровно один период с closed_at IS NULL), а не
	// ошибка вызывающего: наружу уходит обезличенное сообщение.
	{domain.ErrOpenPeriodMissing, codes.Internal, msgInternal},
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
	for _, mapping := range errorMapping {
		if errors.Is(err, mapping.sentinel) {
			return status.Error(mapping.code, mapping.message)
		}
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		switch {
		case errors.Is(ctxErr, context.Canceled):
			return status.Error(codes.Canceled, "запрос отменён вызывающим")
		case errors.Is(ctxErr, context.DeadlineExceeded):
			return status.Error(codes.DeadlineExceeded, "истёк дедлайн запроса")
		}
	}

	return status.Error(codes.Internal, msgInternal)
}
