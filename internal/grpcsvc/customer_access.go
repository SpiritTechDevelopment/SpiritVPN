// Package grpcsvc — транспортный адаптер внешнего gRPC API (§5).
//
// Пакет намеренно тонкий: он переводит protobuf-сообщения в доменные команды,
// вызывает use case и отображает ошибки в gRPC-статусы. Правил §5 здесь нет —
// валидация живёт в domain.ValidateApplyCommand, порядок команд и транзакция в
// app.ApplyCustomerAccess. Дублировать их на транспорте нельзя: два источника
// одного правила со временем расходятся.
//
// Авторизация (§14: mTLS и роль customer-access-writer) хендлерам не видна — её
// выполняет interceptor до входа в метод.
//
// GetCustomerAccessLinks не реализован: он идёт следующим срезом вместе с VLESS
// URI (§8) и пока отвечает Unimplemented из встроенного
// UnimplementedCustomerAccessServiceServer.
package grpcsvc

import (
	"context"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	customerv1 "github.com/RomanRyabinkin/SpiritVPN/internal/gen/spiritvpn/customer/v1"
)

// applyCustomerAccess — use case, который обслуживает метод.
//
// Узкий интерфейс вместо *app.ApplyCustomerAccess: транспорту нужен ровно один
// вызов, а тест хендлера не должен тащить за собой репозиторий, sealer и
// генератор идентификаторов.
type applyCustomerAccess interface {
	Execute(ctx context.Context, cmd domain.ApplyCommand) error
}

// CustomerAccessServer реализует CustomerAccessService (§5).
type CustomerAccessServer struct {
	// Встраивание по значению обязательно: сгенерированный
	// RegisterCustomerAccessServiceServer паникует на встраивании указателем.
	customerv1.UnimplementedCustomerAccessServiceServer

	apply applyCustomerAccess
}

// NewCustomerAccessServer собирает транспорт поверх use case.
func NewCustomerAccessServer(apply applyCustomerAccess) *CustomerAccessServer {
	return &CustomerAccessServer{apply: apply}
}

// ApplyCustomerAccess принимает одну команду product-сервиса (§5).
//
// Пустой ответ означает, что desired state и durable agent operations
// зафиксированы в PostgreSQL, но НЕ то, что агенты их уже применили. Он же
// возвращается на поглощённый реордер или повтор команды (§5, правило 2):
// снаружи принятие и идемпотентный no-op неразличимы.
func (s *CustomerAccessServer) ApplyCustomerAccess(
	ctx context.Context,
	req *customerv1.ApplyCustomerAccessRequest,
) (*customerv1.ApplyCustomerAccessResponse, error) {
	if err := s.apply.Execute(ctx, applyCommandFrom(req)); err != nil {
		return nil, statusFromError(ctx, err)
	}

	return &customerv1.ApplyCustomerAccessResponse{}, nil
}

// applyCommandFrom переводит запрос в доменную команду.
//
// Здесь и только здесь закрепляется секундная точность expires_at (решение 11).
// Домен её не проверяет: §5 перечисляет правила валидации явно, и точности среди
// них нет — её гарантирует тип поля expires_at_epoch_sec. Но timestamptz хранит
// микросекунды, поэтому время с наносекундами прочиталось бы из базы строго
// меньшим, и точный повтор команды классифицировался бы как renewal со сбросом
// счётчиков трафика (§5, правило 8).
func applyCommandFrom(req *customerv1.ApplyCustomerAccessRequest) domain.ApplyCommand {
	return domain.ApplyCommand{
		CustomerID:      req.GetCustomerId(),
		FleetID:         req.GetVpnFleetId(),
		UsageQuotaBytes: req.GetUsageQuotaBytes(),
		ExpiresAt:       time.Unix(req.GetExpiresAtEpochSec(), 0).UTC(),
		CommandNumber:   req.GetCommandNumber(),
	}
}
