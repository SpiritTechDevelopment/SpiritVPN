package grpcsvc

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	customerv1 "github.com/RomanRyabinkin/SpiritVPN/internal/gen/spiritvpn/customer/v1"
)

const (
	applyMethod          = customerv1.CustomerAccessService_ApplyCustomerAccess_FullMethodName
	linksMethod          = customerv1.CustomerAccessService_GetCustomerAccessLinks_FullMethodName
	availableNodesMethod = customerv1.CustomerAccessService_ListAvailableNodes_FullMethodName
)

// okHandler — обработчик, который просто фиксирует, что до него дошли.
func okHandler(reached *bool) grpc.UnaryHandler {
	return func(ctx context.Context, _ any) (any, error) {
		if reached != nil {
			*reached = true
		}
		return "ok", nil
	}
}

// tlsContext подделывает уже проверенное mTLS-соединение. Настоящее рукопожатие
// для проверки авторизации не нужно: интересует только то, что interceptor
// делает с содержимым проверенного сертификата.
func tlsContext(cert *x509.Certificate) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{
				VerifiedChains: [][]*x509.Certificate{{cert}},
			},
		},
	})
}

func certDNS(names ...string) *x509.Certificate {
	return &x509.Certificate{DNSNames: names}
}

func certURI(t *testing.T, raw string) *x509.Certificate {
	t.Helper()

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("разбор URI SAN: %v", err)
	}
	return &x509.Certificate{URIs: []*url.URL{u}}
}

func writerOnly() *Authorizer {
	return NewAuthorizer(map[Role][]string{
		RoleCustomerAccessWriter: {"product-svc"},
	})
}

// --- request_id ------------------------------------------------------------

func TestRequestIDPrefersIncomingValue(t *testing.T) {
	ctx := metadata.NewIncomingContext(
		context.Background(),
		metadata.Pairs(requestIDMetadataKey, "abc-123"),
	)

	var got string
	interceptor := RequestIDUnaryInterceptor(func() string { return "generated" })
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: applyMethod},
		func(ctx context.Context, _ any) (any, error) {
			got = RequestIDFromContext(ctx)
			return nil, nil
		})
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}

	if got != "abc-123" {
		t.Errorf("request_id %q, ожидался входящий abc-123 — иначе теряется сквозная корреляция", got)
	}
}

func TestRequestIDGeneratedWhenUnusable(t *testing.T) {
	tests := []struct {
		name string
		md   bool
		raw  string
	}{
		{name: "metadata отсутствует"},
		{name: "пустое значение", md: true, raw: ""},
		{name: "перевод строки", md: true, raw: "abc\nfake-log-entry"},
		{name: "возврат каретки", md: true, raw: "abc\rdef"},
		{name: "пробел", md: true, raw: "abc def"},
		{name: "не-ASCII", md: true, raw: "идентификатор"},
		{name: "слишком длинный", md: true, raw: strings.Repeat("x", maxRequestIDLen+1)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.md {
				ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(requestIDMetadataKey, tc.raw))
			}

			var got string
			interceptor := RequestIDUnaryInterceptor(func() string { return "generated" })
			_, _ = interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: applyMethod},
				func(ctx context.Context, _ any) (any, error) {
					got = RequestIDFromContext(ctx)
					return nil, nil
				})

			if got != "generated" {
				t.Errorf("request_id %q, ожидался сгенерированный", got)
			}
		})
	}
}

// TestRequestIDNeverFailsRequest — непригодный идентификатор не повод отклонять
// команду product-сервиса: корреляция это удобство, а не правило контракта.
func TestRequestIDNeverFailsRequest(t *testing.T) {
	ctx := metadata.NewIncomingContext(
		context.Background(),
		metadata.Pairs(requestIDMetadataKey, "плохой\nid"),
	)

	reached := false
	interceptor := RequestIDUnaryInterceptor(func() string { return "generated" })
	if _, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: applyMethod}, okHandler(&reached)); err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if !reached {
		t.Error("обработчик не был вызван")
	}
}

// --- авторизация -----------------------------------------------------------

// TestAuthorizeAllowsConfiguredIdentity — базовый успешный путь.
func TestAuthorizeAllowsConfiguredIdentity(t *testing.T) {
	reached := false
	interceptor := writerOnly().UnaryInterceptor()

	_, err := interceptor(tlsContext(certDNS("product-svc")), nil,
		&grpc.UnaryServerInfo{FullMethod: applyMethod}, okHandler(&reached))
	if err != nil {
		t.Fatalf("неожиданный отказ: %v", err)
	}
	if !reached {
		t.Error("обработчик не был вызван")
	}
}

// TestAuthorizeAcceptsURISAN — SPIFFE-подобная идентичность в URI SAN.
func TestAuthorizeAcceptsURISAN(t *testing.T) {
	authorizer := NewAuthorizer(map[Role][]string{
		RoleCustomerAccessWriter: {"spiffe://spirit/ns/prod/sa/product"},
	})

	_, err := authorizer.UnaryInterceptor()(
		tlsContext(certURI(t, "spiffe://spirit/ns/prod/sa/product")), nil,
		&grpc.UnaryServerInfo{FullMethod: applyMethod}, okHandler(nil))
	if err != nil {
		t.Fatalf("неожиданный отказ: %v", err)
	}
}

// TestAuthorizeRejectsUnauthenticated — все способы не предъявить идентичность.
func TestAuthorizeRejectsUnauthenticated(t *testing.T) {
	noChain := peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{}},
	})

	tests := []struct {
		name string
		ctx  context.Context
	}{
		{"нет peer", context.Background()},
		{"peer без TLS", peer.NewContext(context.Background(), &peer.Peer{})},
		{"TLS без проверенной цепочки", noChain},
		{"сертификат без SAN", tlsContext(&x509.Certificate{})},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reached := false
			_, err := writerOnly().UnaryInterceptor()(tc.ctx, nil,
				&grpc.UnaryServerInfo{FullMethod: applyMethod}, okHandler(&reached))

			if got := status.Code(err); got != codes.Unauthenticated {
				t.Errorf("код %v, ожидался Unauthenticated", got)
			}
			if reached {
				t.Error("обработчик не должен вызываться при отказе")
			}
		})
	}
}

// TestAuthorizeIgnoresCommonName — CN идентичностью не является.
// Сертификат, у которого нужное имя лежит только в CN, доступа не получает.
func TestAuthorizeIgnoresCommonName(t *testing.T) {
	cert := &x509.Certificate{Subject: pkix.Name{CommonName: "product-svc"}}

	_, err := writerOnly().UnaryInterceptor()(tlsContext(cert), nil,
		&grpc.UnaryServerInfo{FullMethod: applyMethod}, okHandler(nil))

	if got := status.Code(err); got != codes.Unauthenticated {
		t.Errorf("код %v, ожидался Unauthenticated: CN не должен работать как идентичность", got)
	}
}

// TestAuthorizeRejectsEmptyIdentity — целиком: пустая идентичность из
// сертификата не должна совпасть с пустым элементом в списке разрешённых.
func TestAuthorizeRejectsEmptyIdentity(t *testing.T) {
	authorizer := NewAuthorizer(map[Role][]string{
		RoleCustomerAccessWriter: {"", "product-svc"},
	})

	_, err := authorizer.UnaryInterceptor()(tlsContext(certDNS("")), nil,
		&grpc.UnaryServerInfo{FullMethod: applyMethod}, okHandler(nil))

	if got := status.Code(err); got != codes.Unauthenticated {
		t.Errorf("код %v, ожидался Unauthenticated: пустое не должно совпадать с пустым", got)
	}
}

// TestAuthorizeWriterIsNotReader — чтение отдаёт VLESS URI с
// client_uuid, поэтому право писать не даёт права читать.
func TestAuthorizeWriterIsNotReader(t *testing.T) {
	reached := false

	_, err := writerOnly().UnaryInterceptor()(tlsContext(certDNS("product-svc")), nil,
		&grpc.UnaryServerInfo{FullMethod: linksMethod}, okHandler(&reached))

	if got := status.Code(err); got != codes.PermissionDenied {
		t.Errorf("код %v, ожидался PermissionDenied", got)
	}
	if reached {
		t.Error("обработчик не должен вызываться при отказе")
	}
}

// TestAuthorizeGrantsBothRolesWhenListedTwice — обратная сторона разделения ролей:
// сервис получает оба права, будучи явно перечисленным в обоих списках.
func TestAuthorizeGrantsBothRolesWhenListedTwice(t *testing.T) {
	authorizer := NewAuthorizer(map[Role][]string{
		RoleCustomerAccessWriter: {"product-svc"},
		RoleCustomerAccessReader: {"product-svc"},
	})

	for _, method := range []string{applyMethod, linksMethod, availableNodesMethod} {
		if _, err := authorizer.UnaryInterceptor()(tlsContext(certDNS("product-svc")), nil,
			&grpc.UnaryServerInfo{FullMethod: method}, okHandler(nil)); err != nil {
			t.Errorf("метод %s: неожиданный отказ %v", method, err)
		}
	}
}

// TestAuthorizeDeniesUnknownMethod — deny by default. Метод, забытый в
// methodRoles, обязан отказать всем, а не открыться всем.
func TestAuthorizeDeniesUnknownMethod(t *testing.T) {
	reached := false

	_, err := writerOnly().UnaryInterceptor()(tlsContext(certDNS("product-svc")), nil,
		&grpc.UnaryServerInfo{FullMethod: "/spiritvpn.manifest.v1.ManifestService/PublishManifest"},
		okHandler(&reached))

	if got := status.Code(err); got != codes.PermissionDenied {
		t.Errorf("код %v, ожидался PermissionDenied", got)
	}
	if reached {
		t.Error("обработчик не должен вызываться для метода вне таблицы")
	}
}

// TestAuthorizeDoesNotNameRequiredRole — отказ не подсказывает, какого права
// не хватает.
func TestAuthorizeDoesNotNameRequiredRole(t *testing.T) {
	_, err := writerOnly().UnaryInterceptor()(tlsContext(certDNS("outsider-svc")), nil,
		&grpc.UnaryServerInfo{FullMethod: applyMethod}, okHandler(nil))

	if msg := status.Convert(err).Message(); strings.Contains(msg, string(RoleCustomerAccessWriter)) {
		t.Errorf("сообщение %q называет требуемую роль", msg)
	}
}

// --- логирование -----------------------------------------------------------

func logOnce(t *testing.T, ctx context.Context, method string, req any, handlerErr error) map[string]any {
	t.Helper()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	_, _ = LoggingUnaryInterceptor(logger)(ctx, req, &grpc.UnaryServerInfo{FullMethod: method},
		func(context.Context, any) (any, error) { return "ok", handlerErr })

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("запись лога не разбирается как JSON: %v (%s)", err, buf.String())
	}
	return record
}

func TestLoggingRecordsRequiredFields(t *testing.T) {
	ctx := contextWithRequestID(tlsContext(certDNS("product-svc")), "req-1")
	record := logOnce(t, ctx, applyMethod, nil, nil)

	want := map[string]string{
		"method":        applyMethod,
		"grpc_code":     "OK",
		"error_code":    codeOK,
		"request_id":    "req-1",
		"peer_identity": "product-svc",
	}
	for key, expected := range want {
		if got, _ := record[key].(string); got != expected {
			t.Errorf("поле %s = %q, ожидалось %q", key, got, expected)
		}
	}
	if _, ok := record["duration_ms"]; !ok {
		t.Error("нет duration_ms")
	}
}

// TestLoggingRecordsStableErrorCode — в записи стабильный код, а не только
// gRPC-код, который слишком груб (три доменных исхода делят INVALID_ARGUMENT).
func TestLoggingRecordsStableErrorCode(t *testing.T) {
	err := statusFromError(context.Background(), domain.ErrExpiryRegression)
	record := logOnce(t, tlsContext(certDNS("product-svc")), applyMethod, nil, err)

	if got, _ := record["error_code"].(string); got != codeExpiryRegression {
		t.Errorf("error_code %q, ожидался %q", got, codeExpiryRegression)
	}
	if got, _ := record["grpc_code"].(string); got != codes.FailedPrecondition.String() {
		t.Errorf("grpc_code %q, ожидался FailedPrecondition", got)
	}
	if got, _ := record["level"].(string); got != "WARN" {
		t.Errorf("уровень %q, ожидался WARN: отказ по правилам контракта — штатная работа, а не поломка", got)
	}
}

// TestLoggingRecordsPeerIdentityOnAuthFailure — «кому отказали» самое ценное в
// записи об отказе, поэтому идентичность берётся независимо от Authorizer.
func TestLoggingRecordsPeerIdentityOnAuthFailure(t *testing.T) {
	denied := newStatusError(codes.PermissionDenied, codePermissionDenied, "недостаточно прав")
	record := logOnce(t, tlsContext(certDNS("outsider-svc")), linksMethod, nil, denied)

	if got, _ := record["peer_identity"].(string); got != "outsider-svc" {
		t.Errorf("peer_identity %q, ожидался outsider-svc", got)
	}
	if got, _ := record["error_code"].(string); got != codePermissionDenied {
		t.Errorf("error_code %q, ожидался %q", got, codePermissionDenied)
	}
}

// TestLoggingNeverRecordsRequestBody — customer_id допустим только в audit.
func TestLoggingNeverRecordsRequestBody(t *testing.T) {
	req := &customerv1.ApplyCustomerAccessRequest{
		CustomerId:      "cust-секретный-идентификатор",
		VpnFleetId:      7,
		UsageQuotaBytes: 1 << 30,
	}

	record := logOnce(t, tlsContext(certDNS("product-svc")), applyMethod, req, nil)

	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("сериализация записи: %v", err)
	}
	if strings.Contains(string(raw), "cust-секретный-идентификатор") {
		t.Errorf("customer_id попал в лог: %s", raw)
	}
}

// TestLoggingNeverRecordsResponseBody — ответ с URI не логируется.
//
// Регрессионный guard, а не проверка текущего поведения: interceptor тела ответа
// сейчас не пишет. Стоит кому-нибудь добавить в запись resp «для удобства
// отладки» — и открытый client_uuid каждого READY-access окажется в логах,
// откуда его уже не отозвать. Защита типа crypto.ClientUUID на готовой URI не
// работает: внутри неё обычная строка.
func TestLoggingNeverRecordsResponseBody(t *testing.T) {
	const secretUUID = "f81d4fae-7dec-11d0-a765-00a0c91e6bf6"
	uri := "vless://" + secretUUID + "@nl.example.com:443" +
		"?security=reality&encryption=none&pbk=pub&fp=chrome&type=tcp" +
		"&flow=xtls-rprx-vision&sni=www.example.org&sid=ab#Netherlands"

	resp := &customerv1.GetCustomerAccessLinksResponse{
		Links: []*customerv1.CustomerAccessLink{{
			Kind:  customerv1.AccessKind_ACCESS_KIND_FREEDOM,
			State: customerv1.AccessLinkState_ACCESS_LINK_STATE_READY,
			Uri:   &uri,
		}},
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	_, _ = LoggingUnaryInterceptor(logger)(
		contextWithRequestID(tlsContext(certDNS("product-svc")), "req-1"),
		&customerv1.GetCustomerAccessLinksRequest{CustomerId: "cust-1"},
		&grpc.UnaryServerInfo{FullMethod: linksMethod},
		func(context.Context, any) (any, error) { return resp, nil },
	)

	for _, leaked := range []string{secretUUID, "vless://", "nl.example.com", "cust-1"} {
		if strings.Contains(buf.String(), leaked) {
			t.Errorf("в лог попало %q: %s", leaked, buf.String())
		}
	}
}

func TestLevelForSeparatesServerFaultsFromClientErrors(t *testing.T) {
	tests := []struct {
		code codes.Code
		want slog.Level
	}{
		{codes.OK, slog.LevelInfo},
		{codes.Internal, slog.LevelError},
		{codes.Unknown, slog.LevelError},
		{codes.Unavailable, slog.LevelError},
		{codes.DataLoss, slog.LevelError},
		{codes.InvalidArgument, slog.LevelWarn},
		{codes.NotFound, slog.LevelWarn},
		{codes.FailedPrecondition, slog.LevelWarn},
		{codes.PermissionDenied, slog.LevelWarn},
		{codes.Unauthenticated, slog.LevelWarn},
		{codes.Canceled, slog.LevelWarn},
	}

	for _, tc := range tests {
		if got := levelFor(tc.code); got != tc.want {
			t.Errorf("код %v дал уровень %v, ожидался %v", tc.code, got, tc.want)
		}
	}
}

// --- стабильные коды -------------------------------------------------------

// TestStatusErrorSurface — statusError живёт в двух мирах сразу: gRPC достаёт из
// него статус, а обычный код печатает его как error. Второе легко сломать
// незаметно, потому что gRPC об Error() не спрашивает.
func TestStatusErrorSurface(t *testing.T) {
	err := newStatusError(codes.NotFound, codeFleetNotFound, "fleet не найден")

	if err.Error() != "fleet не найден" {
		t.Errorf("Error() = %q, ожидалось сообщение статуса", err.Error())
	}
	if got := status.Code(err); got != codes.NotFound {
		t.Errorf("код %v, ожидался NotFound", got)
	}
}

// TestPeerIdentityEmptyWithoutTLS — лог не должен падать на вызове без TLS.
func TestPeerIdentityEmptyWithoutTLS(t *testing.T) {
	if got := peerIdentity(context.Background()); got != "" {
		t.Errorf("peer_identity %q, ожидалась пустая строка", got)
	}
}

// TestPeerIdentitiesSkipsNilURI — x509.Certificate.URIs заполняет парсер, и
// доверять его содержимому на nil не стоит: разыменование уронило бы процесс на
// каждом вызове такого клиента.
func TestPeerIdentitiesSkipsNilURI(t *testing.T) {
	cert := &x509.Certificate{
		DNSNames: []string{"product-svc"},
		URIs:     []*url.URL{nil},
	}

	ids := peerIdentities(tlsContext(cert))
	if len(ids) != 1 || ids[0] != "product-svc" {
		t.Errorf("идентичности %#v, ожидалась ровно [product-svc]", ids)
	}
}

func TestStableCodeFallsBackToInternal(t *testing.T) {
	if got := stableCode(nil); got != codeOK {
		t.Errorf("nil дал %q, ожидался %q", got, codeOK)
	}
	if got := stableCode(errors.New("посторонняя ошибка")); got != codeInternal {
		t.Errorf("неопознанная ошибка дала %q, ожидался %q", got, codeInternal)
	}
	if got := stableCode(newStatusError(codes.NotFound, codeFleetNotFound, "fleet не найден")); got != codeFleetNotFound {
		t.Errorf("statusError дал %q, ожидался %q", got, codeFleetNotFound)
	}
}
