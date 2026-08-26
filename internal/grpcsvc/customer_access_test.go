package grpcsvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	customerv1 "github.com/RomanRyabinkin/SpiritVPN/internal/gen/spiritvpn/customer/v1"
)

// epochSec — произвольный момент в будущем; тесты сверяют конверсию времени
// именно с ним.
const epochSec int64 = 1_800_000_000

// stubApply — фейковый use case: запоминает полученную команду и возвращает
// заданную ошибку. Транспорт больше ничего от use case не требует.
type stubApply struct {
	calls   int
	request app.ApplyCustomerCommand
	err     error
}

func (s *stubApply) Execute(_ context.Context, request app.ApplyCustomerCommand) error {
	s.calls++
	s.request = request
	return s.err
}

// cmd — доменная часть последней команды. Сокращение: транспортных тестов,
// сверяющих перевод запроса в команду, много, и .request.Command в каждом
// заслоняло бы то, что они проверяют.
func (s *stubApply) cmd() domain.ApplyCommand { return s.request.Command }

// validRequest — запрос, проходящий валидацию. Тесты меняют в нём только то,
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

// TestApplyCustomerAccessMapsDomainErrors — таблица «доменная ошибка → код» для
// исходов командного пути, то есть для всех строк errorMapping, кроме
// ErrCustomerNotFound: её закрывает TestGetCustomerAccessLinksMapsErrors.
// Обёрнутые случаи проверяют, что сопоставление идёт через errors.Is и
// переживает fmt.Errorf в use case.
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
			srv := NewCustomerAccessServer(stub, &stubLinks{})

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
	srv := NewCustomerAccessServer(stub, &stubLinks{})

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

// TestApplyCustomerAccessPinsExpiresAtPrecision пиннит границу
// секундной точности проходит здесь, на транспорте.
//
// timestamptz хранит микросекунды. Наносекунды в expires_at прочитались бы из
// базы строго меньшим значением, и точный повтор команды классифицировался бы
// как renewal со сбросом счётчиков трафика.
func TestApplyCustomerAccessPinsExpiresAtPrecision(t *testing.T) {
	stub := &stubApply{}
	srv := NewCustomerAccessServer(stub, &stubLinks{})

	if _, err := srv.ApplyCustomerAccess(context.Background(), validRequest()); err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}

	got := stub.cmd().ExpiresAt
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

// TestApplyCustomerAccessMapsRequestFields — все пять полей запроса доезжают до
// команды без искажений.
func TestApplyCustomerAccessMapsRequestFields(t *testing.T) {
	stub := &stubApply{}
	srv := NewCustomerAccessServer(stub, &stubLinks{})

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
	if stub.cmd() != want {
		t.Fatalf("команда %+v, ожидалась %+v", stub.cmd(), want)
	}
}

// TestApplyCustomerAccessSuccessReturnsEmptyResponse — успех отвечает пустым
// сообщением, а не nil-указателем.
func TestApplyCustomerAccessSuccessReturnsEmptyResponse(t *testing.T) {
	srv := NewCustomerAccessServer(&stubApply{}, &stubLinks{})

	resp, err := srv.ApplyCustomerAccess(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if resp == nil {
		t.Fatal("ответ nil, ожидалось пустое сообщение")
	}
}

// TestApplyCustomerAccessMapsCallerContext — закрытый контекст
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
			srv := NewCustomerAccessServer(stub, &stubLinks{})

			_, err := srv.ApplyCustomerAccess(tc.ctx(t), validRequest())
			requireCode(t, err, tc.want)
		})
	}
}

// TestApplyCustomerAccessInternalCancellationStaysInternal — вторая половина
// разбора отмены и главная причина, по которой она определяется по контексту, а
// не по тексту ошибки.
//
// Контекст вызывающего жив, а изнутри пришла context.Canceled: отменил себя сам
// backend. Это дефект сервера, и он обязан остаться INTERNAL, иначе будущий
// внутренний таймаут на транзакцию замаскируется под отмену клиентом.
func TestApplyCustomerAccessInternalCancellationStaysInternal(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			stub := &stubApply{err: fmt.Errorf("блокировка entitlement: %w", cause)}
			srv := NewCustomerAccessServer(stub, &stubLinks{})

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
	srv := NewCustomerAccessServer(stub, &stubLinks{})

	_, err := srv.ApplyCustomerAccess(ctx, validRequest())
	requireCode(t, err, codes.FailedPrecondition)
}

// --- GetCustomerAccessLinks -------------------------------------------------

// stubLinks — фейковый read-use case.
type stubLinks struct {
	calls      int
	customerID string
	links      []app.CustomerAccessLink
	err        error
}

func (s *stubLinks) Execute(_ context.Context, customerID string) ([]app.CustomerAccessLink, error) {
	s.calls++
	s.customerID = customerID
	return s.links, s.err
}

// serverStream — минимальная реализация grpc.ServerTransportStream.
//
// Без неё grpc.SetHeader падает: вне настоящего сервера в контексте нет потока.
// Заодно это единственный способ увидеть заголовок ответа в юнит-тесте.
type serverStream struct {
	method    string
	header    metadata.MD
	headerErr error
}

func (s *serverStream) Method() string { return s.method }

func (s *serverStream) SetHeader(md metadata.MD) error {
	if s.headerErr != nil {
		return s.headerErr
	}
	if s.header == nil {
		s.header = metadata.MD{}
	}
	for key, values := range md {
		s.header[key] = append(s.header[key], values...)
	}
	return nil
}

func (s *serverStream) SendHeader(md metadata.MD) error { return s.SetHeader(md) }
func (s *serverStream) SetTrailer(metadata.MD) error    { return nil }

// linksContext подделывает окружение серверного вызова, чтобы хендлер мог
// выставить метадату ответа.
func linksContext() (context.Context, *serverStream) {
	stream := &serverStream{method: customerv1.CustomerAccessService_GetCustomerAccessLinks_FullMethodName}
	return grpc.NewContextWithServerTransportStream(context.Background(), stream), stream
}

// TestGetCustomerAccessLinksMapsStates — таблица «доменное состояние → protobuf».
// Здесь же проверяется контракт optional-полей: причина есть только у BLOCKED,
// URI — только у READY.
func TestGetCustomerAccessLinksMapsStates(t *testing.T) {
	uri := "vless://f81d4fae-7dec-11d0-a765-00a0c91e6bf6@nl.example.com:443?security=reality#NL"

	tests := []struct {
		name       string
		link       app.CustomerAccessLink
		wantKind   customerv1.AccessKind
		wantState  customerv1.AccessLinkState
		wantReason *customerv1.AccessBlockReason
		wantURI    *string
	}{
		{
			name: "READY несёт URI и не несёт причины",
			link: app.CustomerAccessLink{
				Kind:   domain.AccessKindFreedom,
				Status: domain.LinkStatus{State: domain.LinkStateReady},
				URI:    uri,
			},
			wantKind:  customerv1.AccessKind_ACCESS_KIND_FREEDOM,
			wantState: customerv1.AccessLinkState_ACCESS_LINK_STATE_READY,
			wantURI:   &uri,
		},
		{
			name: "BLOCKED по сроку несёт причину и не несёт URI",
			link: app.CustomerAccessLink{
				Kind: domain.AccessKindBridge,
				Status: domain.LinkStatus{
					State:  domain.LinkStateBlocked,
					Reason: domain.BlockReasonTimeExpired,
				},
			},
			wantKind:   customerv1.AccessKind_ACCESS_KIND_BRIDGE,
			wantState:  customerv1.AccessLinkState_ACCESS_LINK_STATE_BLOCKED,
			wantReason: customerv1.AccessBlockReason_ACCESS_BLOCK_REASON_TIME_EXPIRED.Enum(),
		},
		{
			name: "BLOCKED по квоте",
			link: app.CustomerAccessLink{
				Kind: domain.AccessKindFreedom,
				Status: domain.LinkStatus{
					State:  domain.LinkStateBlocked,
					Reason: domain.BlockReasonTrafficQuotaExhausted,
				},
			},
			wantKind:   customerv1.AccessKind_ACCESS_KIND_FREEDOM,
			wantState:  customerv1.AccessLinkState_ACCESS_LINK_STATE_BLOCKED,
			wantReason: customerv1.AccessBlockReason_ACCESS_BLOCK_REASON_TRAFFIC_QUOTA_EXHAUSTED.Enum(),
		},
		{
			name: "PENDING",
			link: app.CustomerAccessLink{
				Kind:   domain.AccessKindFreedom,
				Status: domain.LinkStatus{State: domain.LinkStatePending},
			},
			wantKind:  customerv1.AccessKind_ACCESS_KIND_FREEDOM,
			wantState: customerv1.AccessLinkState_ACCESS_LINK_STATE_PENDING,
		},
		{
			name: "FAILED",
			link: app.CustomerAccessLink{
				Kind:   domain.AccessKindBridge,
				Status: domain.LinkStatus{State: domain.LinkStateFailed},
			},
			wantKind:  customerv1.AccessKind_ACCESS_KIND_BRIDGE,
			wantState: customerv1.AccessLinkState_ACCESS_LINK_STATE_FAILED,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			const (
				quotaBytes    = uint64(10_000)
				consumedBytes = uint64(4_000)
			)
			tc.link.UsageQuotaBytes = quotaBytes
			tc.link.ConsumedBytes = consumedBytes
			srv := NewCustomerAccessServer(&stubApply{}, &stubLinks{links: []app.CustomerAccessLink{tc.link}})

			ctx, _ := linksContext()
			resp, err := srv.GetCustomerAccessLinks(ctx, &customerv1.GetCustomerAccessLinksRequest{CustomerId: "cust-1"})
			if err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}
			if len(resp.GetLinks()) != 1 {
				t.Fatalf("ссылок %d, ожидалась 1", len(resp.GetLinks()))
			}

			got := resp.GetLinks()[0]
			if got.GetKind() != tc.wantKind {
				t.Errorf("kind %v, ожидался %v", got.GetKind(), tc.wantKind)
			}
			if got.GetState() != tc.wantState {
				t.Errorf("state %v, ожидался %v", got.GetState(), tc.wantState)
			}

			switch {
			case tc.wantReason == nil && got.BlockReason != nil:
				t.Errorf("причина %v при состоянии %v, ожидалось отсутствие", got.GetBlockReason(), tc.wantState)
			case tc.wantReason != nil && got.BlockReason == nil:
				t.Errorf("причина отсутствует, ожидалась %v", *tc.wantReason)
			case tc.wantReason != nil && got.GetBlockReason() != *tc.wantReason:
				t.Errorf("причина %v, ожидалась %v", got.GetBlockReason(), *tc.wantReason)
			}

			switch {
			case tc.wantURI == nil && got.Uri != nil:
				t.Errorf("URI %q при состоянии %v, ожидалось отсутствие", got.GetUri(), tc.wantState)
			case tc.wantURI != nil && got.GetUri() != *tc.wantURI:
				t.Errorf("URI %q, ожидался %q", got.GetUri(), *tc.wantURI)
			}

			if got.UsageQuotaBytes == nil || got.GetUsageQuotaBytes() != quotaBytes {
				t.Errorf("usage_quota_bytes = %v/%d, ожидалось присутствующее %d",
					got.UsageQuotaBytes, got.GetUsageQuotaBytes(), quotaBytes)
			}
			if got.ConsumedBytes == nil || got.GetConsumedBytes() != consumedBytes {
				t.Errorf("consumed_bytes = %v/%d, ожидалось присутствующее %d",
					got.ConsumedBytes, got.GetConsumedBytes(), consumedBytes)
			}
		})
	}
}

// TestGetCustomerAccessLinksSetsNoStore — ответ с URI не кешируется.
func TestGetCustomerAccessLinksSetsNoStore(t *testing.T) {
	srv := NewCustomerAccessServer(&stubApply{}, &stubLinks{})

	ctx, stream := linksContext()
	if _, err := srv.GetCustomerAccessLinks(ctx, &customerv1.GetCustomerAccessLinksRequest{CustomerId: "cust-1"}); err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}

	got := stream.header.Get(headerCacheControl)
	if len(got) != 1 || got[0] != cacheControlNoStore {
		t.Fatalf("заголовок %s = %v, ожидалось [%s]", headerCacheControl, got, cacheControlNoStore)
	}
}

// TestGetCustomerAccessLinksFailsWithoutNoStore — если запрет кеширования
// выставить не удалось, запрос отклоняется, а не отдаёт URI без него: заголовок
// обязателен на любом ответе с credentials. До use case дело при этом не доходит.
func TestGetCustomerAccessLinksFailsWithoutNoStore(t *testing.T) {
	stub := &stubLinks{}
	srv := NewCustomerAccessServer(&stubApply{}, stub)

	stream := &serverStream{headerErr: errors.New("поток закрыт")}
	ctx := grpc.NewContextWithServerTransportStream(context.Background(), stream)

	_, err := srv.GetCustomerAccessLinks(ctx, &customerv1.GetCustomerAccessLinksRequest{CustomerId: "cust-1"})
	requireCode(t, err, codes.Internal)

	if stub.calls != 0 {
		t.Fatalf("use case вызван %d раз, ожидалось 0", stub.calls)
	}
}

// TestGetCustomerAccessLinksPassesCustomerID — идентичность доезжает до use case
// без искажений; своей валидации транспорт не делает (она в домене).
func TestGetCustomerAccessLinksPassesCustomerID(t *testing.T) {
	stub := &stubLinks{}
	srv := NewCustomerAccessServer(&stubApply{}, stub)

	ctx, _ := linksContext()
	if _, err := srv.GetCustomerAccessLinks(ctx, &customerv1.GetCustomerAccessLinksRequest{CustomerId: "cust-42"}); err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}

	if stub.calls != 1 {
		t.Fatalf("use case вызван %d раз, ожидался 1", stub.calls)
	}
	if stub.customerID != "cust-42" {
		t.Fatalf("customer_id %q, ожидался cust-42", stub.customerID)
	}
}

// TestGetCustomerAccessLinksMapsErrors — неизвестный customer даёт NOT_FOUND,
// пустой — INVALID_ARGUMENT, всё остальное обезличивается.
func TestGetCustomerAccessLinksMapsErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"неизвестный customer", domain.ErrCustomerNotFound, codes.NotFound},
		{"обёрнутый неизвестный customer", fmt.Errorf("чтение entitlement: %w", domain.ErrCustomerNotFound), codes.NotFound},
		{"пустой customer_id", domain.ErrCustomerIDInvalid, codes.InvalidArgument},
		{"отказ базы", errors.New(`pq: connection refused (host=db.internal)`), codes.Internal},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewCustomerAccessServer(&stubApply{}, &stubLinks{err: tc.err})

			ctx, _ := linksContext()
			resp, err := srv.GetCustomerAccessLinks(ctx, &customerv1.GetCustomerAccessLinksRequest{})
			if resp != nil {
				t.Fatalf("при ошибке ответ обязан быть nil, получен %v", resp)
			}
			st := requireCode(t, err, tc.want)

			if tc.want == codes.Internal && st.Message() != msgInternal {
				t.Fatalf("сообщение %q, ожидалось обезличенное %q", st.Message(), msgInternal)
			}
		})
	}
}

// TestGetCustomerAccessLinksEmptyList — customer без ссылок отвечает пустым
// списком, а не NOT_FOUND: NOT_FOUND означает отсутствие самого customer.
func TestGetCustomerAccessLinksEmptyList(t *testing.T) {
	srv := NewCustomerAccessServer(&stubApply{}, &stubLinks{})

	ctx, _ := linksContext()
	resp, err := srv.GetCustomerAccessLinks(ctx, &customerv1.GetCustomerAccessLinksRequest{CustomerId: "cust-1"})
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if resp == nil {
		t.Fatal("ответ nil, ожидалось сообщение с пустым списком")
	}
	if len(resp.GetLinks()) != 0 {
		t.Fatalf("ссылок %d, ожидалось 0", len(resp.GetLinks()))
	}
}

// TestGetCustomerAccessLinksRejectsUnknownEnum — расхождение домена и proto
// обязано стать ошибкой, а не уехать клиенту как UNSPECIFIED.
func TestGetCustomerAccessLinksRejectsUnknownEnum(t *testing.T) {
	links := []app.CustomerAccessLink{
		{Kind: "SOMETHING_NEW", Status: domain.LinkStatus{State: domain.LinkStateReady}},
		{Kind: domain.AccessKindFreedom, Status: domain.LinkStatus{State: "SOMETHING_NEW"}},
		{Kind: domain.AccessKindFreedom, Status: domain.LinkStatus{State: domain.LinkStateBlocked}},
	}

	for _, link := range links {
		srv := NewCustomerAccessServer(&stubApply{}, &stubLinks{links: []app.CustomerAccessLink{link}})

		ctx, _ := linksContext()
		_, err := srv.GetCustomerAccessLinks(ctx, &customerv1.GetCustomerAccessLinksRequest{CustomerId: "cust-1"})
		requireCode(t, err, codes.Internal)
	}
}
