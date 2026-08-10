package grpcsvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	customerv1 "github.com/RomanRyabinkin/SpiritVPN/internal/gen/spiritvpn/customer/v1"
)

// epochSec — произвольный момент в будущем; тесты сверяют конверсию времени
// именно с ним.
const epochSec int64 = 1_800_000_000

// stubApply — фейковый use case: запоминает полученную команду и возвращает
// заданную ошибку. Транспорт больше ничего от use case не требует.
type stubApply struct {
	calls int
	cmd   domain.ApplyCommand
	err   error
}

func (s *stubApply) Execute(_ context.Context, cmd domain.ApplyCommand) error {
	s.calls++
	s.cmd = cmd
	return s.err
}

// validRequest — запрос, проходящий валидацию §5. Тесты меняют в нём только то,
// что проверяют.
func validRequest() *customerv1.ApplyCustomerAccessRequest {
	return &customerv1.ApplyCustomerAccessRequest{
		CustomerId:        "cust-1",
		VpnFleetId:        7,
		UsageQuotaBytes:   1 << 30,
		ExpiresAtEpochSec: epochSec,
		CommandNumber:     3,
	}
}

func requireCode(t *testing.T, err error, want codes.Code) *status.Status {
	t.Helper()

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("ошибка не является gRPC-статусом: %v", err)
	}
	if st.Code() != want {
		t.Fatalf("код %v, ожидался %v (сообщение %q)", st.Code(), want, st.Message())
	}
	return st
}

// TestApplyCustomerAccessMapsDomainErrors — таблица «доменная ошибка → код»
// (§5, шапка domain/errors.go). Обёрнутые случаи проверяют, что сопоставление
// идёт через errors.Is и переживает fmt.Errorf в use case.
func TestApplyCustomerAccessMapsDomainErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"пустой customer_id", domain.ErrCustomerIDInvalid, codes.InvalidArgument},
		{"неположительный fleet_id", domain.ErrFleetIDInvalid, codes.InvalidArgument},
		{"неположительная квота", domain.ErrQuotaInvalid, codes.InvalidArgument},
		{"неположительный command_number", domain.ErrCommandNumberInvalid, codes.InvalidArgument},
		{"expiry не в будущем", domain.ErrExpiryNotInFuture, codes.InvalidArgument},
		{"неизвестный fleet", domain.ErrFleetNotFound, codes.NotFound},
		{"другой fleet", domain.ErrFleetMismatch, codes.FailedPrecondition},
		{"сокращение expiry", domain.ErrExpiryRegression, codes.FailedPrecondition},
		{"нет открытого периода", domain.ErrOpenPeriodMissing, codes.Internal},

		{
			name: "обёрнутый сентинел",
			err:  fmt.Errorf("поиск fleet: %w", domain.ErrFleetNotFound),
			want: codes.NotFound,
		},
		{
			name: "дважды обёрнутый сентинел",
			err:  fmt.Errorf("транзакция: %w", fmt.Errorf("чтение access: %w", domain.ErrFleetMismatch)),
			want: codes.FailedPrecondition,
		},
		{
			name: "неопознанная ошибка",
			err:  errors.New("что-то пошло не так"),
			want: codes.Internal,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubApply{err: tc.err}
			srv := NewCustomerAccessServer(stub)

			resp, err := srv.ApplyCustomerAccess(context.Background(), validRequest())
			if resp != nil {
				t.Fatalf("при ошибке ответ обязан быть nil, получен %v", resp)
			}
			requireCode(t, err, tc.want)
		})
	}
}

// TestApplyCustomerAccessHidesInfrastructureDetails — детали БД наружу не
// утекают даже когда use case обернул ошибку драйвера своим контекстом.
func TestApplyCustomerAccessHidesInfrastructureDetails(t *testing.T) {
	const driverDetail = `pq: relation "vpn_accesses" does not exist (host=db.internal user=spirit)`

	stub := &stubApply{err: fmt.Errorf("чтение access: %w", errors.New(driverDetail))}
	srv := NewCustomerAccessServer(stub)

	_, err := srv.ApplyCustomerAccess(context.Background(), validRequest())
	st := requireCode(t, err, codes.Internal)

	for _, leaked := range []string{"pq:", "vpn_accesses", "db.internal", "spirit"} {
		if strings.Contains(st.Message(), leaked) {
			t.Fatalf("сообщение %q содержит деталь инфраструктуры %q", st.Message(), leaked)
		}
	}
	if st.Message() != msgInternal {
		t.Fatalf("сообщение %q, ожидалось обезличенное %q", st.Message(), msgInternal)
	}
}

// TestApplyCustomerAccessPinsExpiresAtPrecision пиннит решение 11: граница
// секундной точности проходит здесь, на транспорте.
//
// timestamptz хранит микросекунды. Наносекунды в expires_at прочитались бы из
// базы строго меньшим значением, и точный повтор команды классифицировался бы
// как renewal со сбросом счётчиков трафика (§5, правило 8).
func TestApplyCustomerAccessPinsExpiresAtPrecision(t *testing.T) {
	stub := &stubApply{}
	srv := NewCustomerAccessServer(stub)

	if _, err := srv.ApplyCustomerAccess(context.Background(), validRequest()); err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}

	got := stub.cmd.ExpiresAt
	if want := time.Unix(epochSec, 0).UTC(); !got.Equal(want) {
		t.Fatalf("expires_at %v, ожидалось %v", got, want)
	}
	if got.Nanosecond() != 0 {
		t.Fatalf("expires_at содержит наносекунды: %v", got)
	}
	if got.Location() != time.UTC {
		t.Fatalf("expires_at в зоне %v, ожидался UTC", got.Location())
	}
}

// TestApplyCustomerAccessMapsRequestFields — все пять полей §5 доезжают до
// команды без искажений.
func TestApplyCustomerAccessMapsRequestFields(t *testing.T) {
	stub := &stubApply{}
	srv := NewCustomerAccessServer(stub)

	req := validRequest()
	if _, err := srv.ApplyCustomerAccess(context.Background(), req); err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}

	if stub.calls != 1 {
		t.Fatalf("use case вызван %d раз, ожидался 1", stub.calls)
	}

	want := domain.ApplyCommand{
		CustomerID:      req.GetCustomerId(),
		FleetID:         req.GetVpnFleetId(),
		UsageQuotaBytes: req.GetUsageQuotaBytes(),
		ExpiresAt:       time.Unix(req.GetExpiresAtEpochSec(), 0).UTC(),
		CommandNumber:   req.GetCommandNumber(),
	}
	if stub.cmd != want {
		t.Fatalf("команда %+v, ожидалась %+v", stub.cmd, want)
	}
}

// TestApplyCustomerAccessSuccessReturnsEmptyResponse — успех отвечает пустым
// сообщением, а не nil-указателем (§5).
func TestApplyCustomerAccessSuccessReturnsEmptyResponse(t *testing.T) {
	srv := NewCustomerAccessServer(&stubApply{})

	resp, err := srv.ApplyCustomerAccess(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if resp == nil {
		t.Fatal("ответ nil, ожидалось пустое сообщение")
	}
}

// TestApplyCustomerAccessMapsCallerContext — решение 12: закрытый контекст
// вызывающего не выдаётся за отказ backend.
func TestApplyCustomerAccessMapsCallerContext(t *testing.T) {
	tests := []struct {
		name    string
		ctx     func(t *testing.T) context.Context
		cause   error
		want    codes.Code
		message string
	}{
		{
			name: "вызывающий отменил",
			ctx: func(*testing.T) context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			cause: context.Canceled,
			want:  codes.Canceled,
		},
		{
			name: "истёк дедлайн вызывающего",
			ctx: func(t *testing.T) context.Context {
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				t.Cleanup(cancel)
				return ctx
			},
			cause: context.DeadlineExceeded,
			want:  codes.DeadlineExceeded,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubApply{err: fmt.Errorf("блокировка entitlement: %w", tc.cause)}
			srv := NewCustomerAccessServer(stub)

			_, err := srv.ApplyCustomerAccess(tc.ctx(t), validRequest())
			requireCode(t, err, tc.want)
		})
	}
}

// TestApplyCustomerAccessInternalCancellationStaysInternal — вторая половина
// решения 12 и главная причина, по которой отмена определяется по контексту, а
// не по тексту ошибки.
//
// Контекст вызывающего жив, а изнутри пришла context.Canceled: отменил себя сам
// backend. Это дефект сервера, и он обязан остаться INTERNAL, иначе будущий
// внутренний таймаут на транзакцию замаскируется под отмену клиентом.
func TestApplyCustomerAccessInternalCancellationStaysInternal(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			stub := &stubApply{err: fmt.Errorf("блокировка entitlement: %w", cause)}
			srv := NewCustomerAccessServer(stub)

			_, err := srv.ApplyCustomerAccess(context.Background(), validRequest())
			requireCode(t, err, codes.Internal)
		})
	}
}

// TestApplyCustomerAccessDomainErrorBeatsClosedContext — порядок проверок в
// statusFromError. Вызывающий отменился сразу после того, как команда была
// отклонена по существу; исход обязан сохранить доменную разметку, а не
// превратиться в CANCELED.
func TestApplyCustomerAccessDomainErrorBeatsClosedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stub := &stubApply{err: fmt.Errorf("классификация: %w", domain.ErrExpiryRegression)}
	srv := NewCustomerAccessServer(stub)

	_, err := srv.ApplyCustomerAccess(ctx, validRequest())
	requireCode(t, err, codes.FailedPrecondition)
}

// TestGetCustomerAccessLinksUnimplemented — метод придёт следующим срезом вместе
// с VLESS URI (§8); до тех пор он обязан честно отвечать Unimplemented.
func TestGetCustomerAccessLinksUnimplemented(t *testing.T) {
	srv := NewCustomerAccessServer(&stubApply{})

	_, err := srv.GetCustomerAccessLinks(context.Background(), &customerv1.GetCustomerAccessLinksRequest{})
	requireCode(t, err, codes.Unimplemented)
}
