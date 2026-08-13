package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/config"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	customerv1 "github.com/RomanRyabinkin/SpiritVPN/internal/gen/spiritvpn/customer/v1"
	manifestv1 "github.com/RomanRyabinkin/SpiritVPN/internal/gen/spiritvpn/manifest/v1"
)

// Этот файл проверяет границу безопасности целиком: настоящее TLS-рукопожатие,
// настоящую проверку цепочки до CA и разбор SAN из настоящего сертификата.
//
// Юнит-тесты interceptor'ов подделывают peer.Peer уже готовым x509.Certificate и
// потому не проверяют ни рукопожатие, ни RequireAndVerifyClientCert — то есть
// ровно тот код, ошибка в котором означает открытый наружу метод.
//
// PostgreSQL здесь не нужен: use case подменён заглушкой, проверяется вход в
// систему, а не путь до базы.

const rpcTimeout = 5 * time.Second

// stubUseCase подменяет ApplyCustomerAccess. Мьютекс нужен, потому что вызовы
// приходят из горутин gRPC-сервера.
type stubUseCase struct {
	mu     sync.Mutex
	calls  int
	cmd    app.ApplyCustomerCommand
	result error
}

func (s *stubUseCase) Execute(_ context.Context, cmd app.ApplyCustomerCommand) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.calls++
	s.cmd = cmd
	return s.result
}

func (s *stubUseCase) state() (int, app.ApplyCustomerCommand) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.calls, s.cmd
}

// stubLinksUseCase подменяет GetCustomerAccessLinks.
type stubLinksUseCase struct {
	mu     sync.Mutex
	calls  int
	links  []app.CustomerAccessLink
	result error
}

func (s *stubLinksUseCase) Execute(_ context.Context, _ string) ([]app.CustomerAccessLink, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.calls++
	return s.links, s.result
}

// stubManifestUseCase подменяет ApplyFleetManifest.
type stubManifestUseCase struct {
	mu     sync.Mutex
	calls  int
	cmd    app.ApplyManifestCommand
	result app.ApplyManifestResult
	err    error
}

func (s *stubManifestUseCase) Execute(
	_ context.Context,
	cmd app.ApplyManifestCommand,
) (app.ApplyManifestResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.calls++
	s.cmd = cmd
	return s.result, s.err
}

func (s *stubManifestUseCase) state() (int, app.ApplyManifestCommand) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.calls, s.cmd
}

func (s *stubLinksUseCase) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.calls
}

// certAuthority — одноразовый CA теста.
type certAuthority struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
	dir     string
	serial  int64
}

func newCertAuthority(t *testing.T) *certAuthority {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ключ CA: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "spiritvpn-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("сертификат CA: %v", err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("разбор сертификата CA: %v", err)
	}

	ca := &certAuthority{
		cert:    cert,
		key:     key,
		certPEM: pemBytes("CERTIFICATE", der),
		dir:     t.TempDir(),
		serial:  1,
	}

	ca.writeFile(t, "ca.crt", ca.certPEM)
	return ca
}

// issue выпускает сертификат, подписанный CA.
//
// Идентичность кладётся в DNS SAN, а в CN пишется заведомо другое значение:
// авторизация обязана читать SAN и игнорировать CN, и
// расхождение делает нарушение этого правила видимым сразу.
func (ca *certAuthority) issue(
	t *testing.T,
	name string,
	dnsNames []string,
	ips []net.IP,
	usage x509.ExtKeyUsage,
) tls.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ключ %s: %v", name, err)
	}

	ca.serial++
	template := &x509.Certificate{
		SerialNumber: big.NewInt(ca.serial),
		Subject:      pkix.Name{CommonName: "cn-не-идентичность"},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("сертификат %s: %v", name, err)
	}

	encodedKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("сериализация ключа %s: %v", name, err)
	}

	pair, err := tls.X509KeyPair(pemBytes("CERTIFICATE", der), pemBytes("EC PRIVATE KEY", encodedKey))
	if err != nil {
		t.Fatalf("пара %s: %v", name, err)
	}
	return pair
}

// issueServer кладёт серверную пару в файлы: transportCredentials читает её с
// диска, и проверять надо именно этот путь.
func (ca *certAuthority) issueServer(t *testing.T) (certPath, keyPath string) {
	t.Helper()

	pair := ca.issue(t, "server",
		[]string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, x509.ExtKeyUsageServerAuth)

	certPath = filepath.Join(ca.dir, "server.crt")
	keyPath = filepath.Join(ca.dir, "server.key")

	ca.writeFile(t, "server.crt", pemBytes("CERTIFICATE", pair.Certificate[0]))

	encoded, err := x509.MarshalECPrivateKey(pair.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		t.Fatalf("сериализация серверного ключа: %v", err)
	}
	ca.writeFile(t, "server.key", pemBytes("EC PRIVATE KEY", encoded))

	return certPath, keyPath
}

func (ca *certAuthority) writeFile(t *testing.T, name string, content []byte) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(ca.dir, name), content, 0o600); err != nil {
		t.Fatalf("запись %s: %v", name, err)
	}
}

// testServer — поднятый сервер и всё, что нужно тестам вокруг него.
type testServer struct {
	addr     string
	ca       *certAuthority
	stub     *stubUseCase
	links    *stubLinksUseCase
	manifest *stubManifestUseCase
	logs     *bytes.Buffer
	logMu    *sync.Mutex
}

// syncBuffer сериализует запись: логгер вызывается из горутин gRPC.
type syncBuffer struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (s syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.buf.Write(p)
}

func startServer(t *testing.T, writers, readers []string) *testServer {
	return startServerWithManifest(t, writers, readers, nil)
}

func startServerWithManifest(t *testing.T, writers, readers, manifestWriters []string) *testServer {
	t.Helper()

	ca := newCertAuthority(t)
	certPath, keyPath := ca.issueServer(t)

	var (
		logs  = &bytes.Buffer{}
		logMu = &sync.Mutex{}
	)
	logger := slog.New(slog.NewJSONHandler(syncBuffer{mu: logMu, buf: logs},
		&slog.HandlerOptions{Level: slog.LevelDebug}))

	stub := &stubUseCase{}
	links := &stubLinksUseCase{}
	manifest := &stubManifestUseCase{}
	server, err := newGRPCServer(config.GRPC{
		CertFile:              certPath,
		KeyFile:               keyPath,
		ClientCAFile:          filepath.Join(ca.dir, "ca.crt"),
		CustomerAccessWriters: writers,
		CustomerAccessReaders: readers,
		ManifestWriters:       manifestWriters,
	}, logger, stub, links, manifest)
	if err != nil {
		t.Fatalf("сборка сервера: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("слушатель: %v", err)
	}

	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	return &testServer{
		addr:     listener.Addr().String(),
		ca:       ca,
		stub:     stub,
		links:    links,
		manifest: manifest,
		logs:     logs,
		logMu:    logMu,
	}
}

// client дозванивается до сервера как customer-сервис.
func (s *testServer) client(t *testing.T, identity string) customerv1.CustomerAccessServiceClient {
	t.Helper()

	return customerv1.NewCustomerAccessServiceClient(s.conn(t, identity))
}

// manifestClient дозванивается до сервера как infrastructure CI/CD.
func (s *testServer) manifestClient(t *testing.T, identity string) manifestv1.ManifestServiceClient {
	t.Helper()

	return manifestv1.NewManifestServiceClient(s.conn(t, identity))
}

// conn поднимает соединение с указанной идентичностью. Пустой identity означает
// вызов вовсе без клиентского сертификата.
func (s *testServer) conn(t *testing.T, identity string) *grpc.ClientConn {
	t.Helper()

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(s.ca.certPEM) {
		t.Fatal("CA теста не разобрался")
	}

	tlsCfg := &tls.Config{
		RootCAs:    roots,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS13,
	}
	if identity != "" {
		tlsCfg.Certificates = []tls.Certificate{
			s.ca.issue(t, identity, []string{identity}, nil, x509.ExtKeyUsageClientAuth),
		}
	}

	conn, err := grpc.NewClient(s.addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	if err != nil {
		t.Fatalf("подключение: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

func (s *testServer) logLines() []map[string]any {
	s.logMu.Lock()
	defer s.logMu.Unlock()

	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(s.logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err == nil {
			records = append(records, record)
		}
	}
	return records
}

func validApply() *customerv1.ApplyCustomerAccessRequest {
	return &customerv1.ApplyCustomerAccessRequest{
		CustomerId:        "cust-1",
		VpnFleetId:        1,
		UsageQuotaBytes:   1 << 30,
		ExpiresAtEpochSec: time.Now().Add(24 * time.Hour).Unix(),
		CommandNumber:     1,
	}
}

func callContext(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	t.Cleanup(cancel)
	return ctx
}

// TestMTLSAllowsConfiguredWriter — штатный путь: рукопожатие, идентичность из
// DNS SAN, роль writer, хендлер, use case.
func TestMTLSAllowsConfiguredWriter(t *testing.T) {
	server := startServer(t, []string{"product-svc"}, nil)

	resp, err := server.client(t, "product-svc").
		ApplyCustomerAccess(callContext(t), validApply())
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if resp == nil {
		t.Fatal("пустой ответ не получен")
	}

	calls, request := server.stub.state()
	if calls != 1 {
		t.Fatalf("use case вызван %d раз, ожидался 1", calls)
	}

	cmd := request.Command
	if cmd.CustomerID != "cust-1" || cmd.FleetID != 1 {
		t.Errorf("команда доехала искажённой: %+v", cmd)
	}
	// Секундная точность expires_at на настоящем транспорте.
	if cmd.ExpiresAt.Nanosecond() != 0 || cmd.ExpiresAt.Location() != time.UTC {
		t.Errorf("expires_at %v: ожидалась секундная точность в UTC", cmd.ExpiresAt)
	}

	// Идентичность вызывающего из mTLS уезжает в audit_events. Проверяется
	// на настоящем рукопожатии, потому что это единственное место, где она
	// вообще появляется.
	if request.Actor != "product-svc" {
		t.Errorf("actor %q, ожидался product-svc", request.Actor)
	}
	if request.RequestID == "" {
		t.Error("request_id не доехал до use case: запись аудита не свяжется с логами")
	}
}

// TestMTLSPropagatesDomainError — доменный исход доезжает до клиента своим
// кодом, а не превращается в INTERNAL по дороге через транспорт.
func TestMTLSPropagatesDomainError(t *testing.T) {
	server := startServer(t, []string{"product-svc"}, nil)
	server.stub.result = domain.ErrFleetNotFound

	_, err := server.client(t, "product-svc").
		ApplyCustomerAccess(callContext(t), validApply())

	if got := status.Code(err); got != codes.NotFound {
		t.Fatalf("код %v, ожидался NotFound", got)
	}
}

// TestMTLSWriterCannotRead — на настоящем соединении. Чтение отдаёт
// VLESS URI с client_uuid, поэтому право писать не даёт права читать.
func TestMTLSWriterCannotRead(t *testing.T) {
	server := startServer(t, []string{"product-svc"}, nil)

	_, err := server.client(t, "product-svc").
		GetCustomerAccessLinks(callContext(t), &customerv1.GetCustomerAccessLinksRequest{CustomerId: "cust-1"})

	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("код %v, ожидался PermissionDenied: writer получил права reader", got)
	}
}

// TestMTLSReaderGetsLinksWithNoStore — read-путь целиком на настоящем
// соединении: identity с ролью reader доходит до хендлера, ответ несёт URI и
// запрет кеширования.
func TestMTLSReaderGetsLinksWithNoStore(t *testing.T) {
	const uri = "vless://f81d4fae-7dec-11d0-a765-00a0c91e6bf6@nl.example.com:443?security=reality#NL"

	server := startServer(t, nil, []string{"product-svc"})
	server.links.links = []app.CustomerAccessLink{{
		Kind:   domain.AccessKindFreedom,
		Status: domain.LinkStatus{State: domain.LinkStateReady},
		URI:    uri,
	}}

	var header metadata.MD
	resp, err := server.client(t, "product-svc").GetCustomerAccessLinks(
		callContext(t),
		&customerv1.GetCustomerAccessLinksRequest{CustomerId: "cust-1"},
		grpc.Header(&header),
	)
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}

	if server.links.callCount() != 1 {
		t.Fatalf("use case вызван %d раз, ожидался 1", server.links.callCount())
	}
	if got := resp.GetLinks(); len(got) != 1 || got[0].GetUri() != uri {
		t.Fatalf("ссылки %v, ожидалась одна с URI %q", got, uri)
	}
	if got := header.Get("cache-control"); len(got) != 1 || got[0] != "no-store" {
		t.Fatalf("cache-control %v, ожидалось [no-store]", got)
	}
}

// TestMTLSNeverLogsIssuedURI — на настоящем соединении: ответ с credentials не
// попадает в лог сервера ни через interceptor, ни через сам gRPC.
func TestMTLSNeverLogsIssuedURI(t *testing.T) {
	const secretUUID = "f81d4fae-7dec-11d0-a765-00a0c91e6bf6"

	server := startServer(t, nil, []string{"product-svc"})
	server.links.links = []app.CustomerAccessLink{{
		Kind:   domain.AccessKindFreedom,
		Status: domain.LinkStatus{State: domain.LinkStateReady},
		URI:    "vless://" + secretUUID + "@nl.example.com:443?security=reality#NL",
	}}

	if _, err := server.client(t, "product-svc").GetCustomerAccessLinks(
		callContext(t),
		&customerv1.GetCustomerAccessLinksRequest{CustomerId: "cust-секретный"},
	); err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}

	server.logMu.Lock()
	defer server.logMu.Unlock()

	for _, leaked := range []string{secretUUID, "vless://", "cust-секретный"} {
		if strings.Contains(server.logs.String(), leaked) {
			t.Errorf("в лог сервера попало %q: %s", leaked, server.logs.String())
		}
	}
}

// TestMTLSManifestWriterIsSeparateRole — на настоящем соединении роль
// manifest-writer отдельна от customer-ролей: манифест переписывает топологию
// целиком, и права продуктового сервиса на него не распространяются ни в какую
// сторону.
func TestMTLSManifestWriterIsSeparateRole(t *testing.T) {
	server := startServerWithManifest(t,
		[]string{"product-svc"}, []string{"product-svc"}, []string{"infra-ci"})

	// Полноправный customer-сервис манифест применить не может.
	_, err := server.manifestClient(t, "product-svc").
		ApplyFleetManifest(callContext(t), &manifestv1.ApplyFleetManifestRequest{})
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("код %v, ожидался PermissionDenied: product-svc получил права manifest-writer", got)
	}
	if calls, _ := server.manifest.state(); calls != 0 {
		t.Errorf("use case вызван %d раз при отказе авторизации", calls)
	}

	// И наоборот: infra-ci не имеет доступа к customer-методам.
	_, err = server.client(t, "infra-ci").ApplyCustomerAccess(callContext(t), validApply())
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("код %v, ожидался PermissionDenied: infra-ci получил права customer-writer", got)
	}
}

// TestMTLSManifestWriterApplies — путь приёма манифеста целиком: identity с
// ролью manifest-writer доходит до хендлера, и она же уезжает в аудит.
func TestMTLSManifestWriterApplies(t *testing.T) {
	server := startServerWithManifest(t, nil, nil, []string{"infra-ci"})
	server.manifest.result = app.ApplyManifestResult{Revision: 42}

	resp, err := server.manifestClient(t, "infra-ci").ApplyFleetManifest(
		callContext(t),
		&manifestv1.ApplyFleetManifestRequest{
			SchemaVersion: domain.ManifestSchemaVersion,
			Revision:      42,
		},
	)
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if resp.GetAppliedRevision() != 42 {
		t.Errorf("applied_revision %d, ожидалась 42", resp.GetAppliedRevision())
	}

	calls, cmd := server.manifest.state()
	if calls != 1 {
		t.Fatalf("use case вызван %d раз, ожидался 1", calls)
	}
	if cmd.Actor != "infra-ci" {
		t.Errorf("actor %q, ожидался infra-ci: аудит получит не ту идентичность", cmd.Actor)
	}
	if cmd.RequestID == "" {
		t.Error("request_id пуст: запись аудита нечем будет скоррелировать с логом")
	}
}

// TestMTLSRejectsUnknownIdentity — сертификат подписан нашим CA, то есть
// рукопожатие проходит, но идентичности нет ни в одном списке.
func TestMTLSRejectsUnknownIdentity(t *testing.T) {
	server := startServer(t, []string{"product-svc"}, nil)

	_, err := server.client(t, "outsider-svc").
		ApplyCustomerAccess(callContext(t), validApply())

	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("код %v, ожидался PermissionDenied", got)
	}

	if calls, _ := server.stub.state(); calls != 0 {
		t.Errorf("use case вызван %d раз при отказе авторизации", calls)
	}
}

// TestMTLSRejectsMissingClientCertificate — и tls.RequireAndVerifyClientCert.
// Отказ обязан случиться на рукопожатии, а не на уровне приложения.
func TestMTLSRejectsMissingClientCertificate(t *testing.T) {
	server := startServer(t, []string{"product-svc"}, nil)

	_, err := server.client(t, "").ApplyCustomerAccess(callContext(t), validApply())
	if err == nil {
		t.Fatal("вызов без клиентского сертификата обязан провалиться")
	}
	if got := status.Code(err); got != codes.Unavailable {
		t.Errorf("код %v, ожидался Unavailable: соединение не должно устанавливаться", got)
	}

	if calls, _ := server.stub.state(); calls != 0 {
		t.Errorf("use case вызван %d раз без клиентского сертификата", calls)
	}
}

// TestMTLSRejectsForeignCA — сертификат правильной формы, но подписанный чужим
// CA. Проверяет, что доверие определяется цепочкой, а не наличием сертификата.
func TestMTLSRejectsForeignCA(t *testing.T) {
	server := startServer(t, []string{"product-svc"}, nil)
	foreign := newCertAuthority(t)

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(server.ca.certPEM) {
		t.Fatal("CA сервера не разобрался")
	}

	conn, err := grpc.NewClient(server.addr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		RootCAs:    roots,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{
			foreign.issue(t, "product-svc", []string{"product-svc"}, nil, x509.ExtKeyUsageClientAuth),
		},
	})))
	if err != nil {
		t.Fatalf("подключение: %v", err)
	}
	defer func() { _ = conn.Close() }()

	_, err = customerv1.NewCustomerAccessServiceClient(conn).
		ApplyCustomerAccess(callContext(t), validApply())
	if err == nil {
		t.Fatal("сертификат чужого CA обязан быть отвергнут")
	}

	if calls, _ := server.stub.state(); calls != 0 {
		t.Errorf("use case вызван %d раз с сертификатом чужого CA", calls)
	}
}

// TestMTLSIgnoresCommonName — на настоящем сертификате. Во всех
// выпущенных здесь сертификатах CN заведомо не совпадает с идентичностью, и
// авторизация обязана опираться только на SAN.
func TestMTLSIgnoresCommonName(t *testing.T) {
	server := startServer(t, []string{"cn-не-идентичность"}, nil)

	_, err := server.client(t, "product-svc").
		ApplyCustomerAccess(callContext(t), validApply())

	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("код %v, ожидался PermissionDenied: CN сработал как идентичность", got)
	}
}

// TestMTLSLogsIdentityAndRequestID — на настоящем вызове: в записи есть
// request_id, стабильный код и то, кому именно отказали.
func TestMTLSLogsIdentityAndRequestID(t *testing.T) {
	server := startServer(t, []string{"product-svc"}, nil)

	_, _ = server.client(t, "outsider-svc").
		ApplyCustomerAccess(callContext(t), validApply())

	records := server.logLines()
	if len(records) == 0 {
		t.Fatal("вызов не оставил записи в логе")
	}

	record := records[len(records)-1]
	checks := map[string]string{
		"peer_identity": "outsider-svc",
		"error_code":    "PERMISSION_DENIED",
		"grpc_code":     "PermissionDenied",
	}
	for key, want := range checks {
		if got, _ := record[key].(string); got != want {
			t.Errorf("поле %s = %q, ожидалось %q (запись: %v)", key, got, want, record)
		}
	}
	if id, _ := record["request_id"].(string); id == "" {
		t.Error("в записи нет request_id")
	}
}

func pemBytes(kind string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der})
}
