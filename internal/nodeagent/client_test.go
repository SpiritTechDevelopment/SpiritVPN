package nodeagent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	nodeagentv1 "github.com/RomanRyabinkin/SpiritVPN/internal/gen/spiritvpn/nodeagent/v1"
)

// Тесты идут против поддельного агента с НАСТОЯЩИМ mTLS-рукопожатием: смысл
// этого пакета целиком в границе доверия, и подделка credentials.TLSInfo не
// проверила бы ни цепочку, ни сверку идентичности — то есть ровно тот код,
// ошибка в котором означает выданные чужой ноде credentials.
//
// Заодно поддельный агент — заготовка conformance-харнесса контракта: логика
// агента живёт в другом репозитории, и обе стороны стоит проверять против одного
// набора ожиданий.

const (
	testAgentIdentity = "spiffe://spiritvpn/node/NL-1"
	testAgentDNSName  = "nl-1.agent.internal"
	testBackendName   = "spiritvpn-backend"
)

// --- удостоверяющий центр -----------------------------------------------------

type testCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
	dir     string
	serial  int64
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ключ CA: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "nodeagent-test-ca"},
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

	ca := &testCA{cert: cert, key: key, certPEM: pemBlock("CERTIFICATE", der), dir: t.TempDir(), serial: 1}
	ca.writeFile(t, "ca.crt", ca.certPEM)
	return ca
}

// issue выпускает сертификат. CN намеренно не совпадает ни с чем: идентичность
// читается только из SAN (решение 13 и его зеркало, решение 36).
func (ca *testCA) issue(
	t *testing.T,
	dnsNames []string,
	uris []string,
	ips []net.IP,
	usage x509.ExtKeyUsage,
) tls.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ключ: %v", err)
	}

	parsed := make([]*url.URL, 0, len(uris))
	for _, raw := range uris {
		u, parseErr := url.Parse(raw)
		if parseErr != nil {
			t.Fatalf("разбор URI SAN %q: %v", raw, parseErr)
		}
		parsed = append(parsed, u)
	}

	ca.serial++
	template := &x509.Certificate{
		SerialNumber: big.NewInt(ca.serial),
		Subject:      pkix.Name{CommonName: "cn-не-идентичность"},
		DNSNames:     dnsNames,
		URIs:         parsed,
		IPAddresses:  ips,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("сертификат: %v", err)
	}
	encodedKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("сериализация ключа: %v", err)
	}

	pair, err := tls.X509KeyPair(pemBlock("CERTIFICATE", der), pemBlock("EC PRIVATE KEY", encodedKey))
	if err != nil {
		t.Fatalf("пара: %v", err)
	}
	return pair
}

// issueBackendFiles кладёт клиентскую пару backend на диск: Client читает её
// оттуда, и проверять надо именно этот путь.
func (ca *testCA) issueBackendFiles(t *testing.T) Config {
	t.Helper()

	pair := ca.issue(t, []string{testBackendName}, nil, nil, x509.ExtKeyUsageClientAuth)

	ca.writeFile(t, "backend.crt", pemBlock("CERTIFICATE", pair.Certificate[0]))
	encoded, err := x509.MarshalECPrivateKey(pair.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		t.Fatalf("сериализация ключа backend: %v", err)
	}
	ca.writeFile(t, "backend.key", pemBlock("EC PRIVATE KEY", encoded))

	return Config{
		CertFile: filepath.Join(ca.dir, "backend.crt"),
		KeyFile:  filepath.Join(ca.dir, "backend.key"),
		CAFile:   filepath.Join(ca.dir, "ca.crt"),
	}
}

func (ca *testCA) writeFile(t *testing.T, name string, content []byte) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(ca.dir, name), content, 0o600); err != nil {
		t.Fatalf("запись %s: %v", name, err)
	}
}

func pemBlock(kind string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der})
}

// --- поддельный агент ---------------------------------------------------------

// fakeAgent реализует mutating-методы контракта и запоминает последний запрос.
type fakeAgent struct {
	nodeagentv1.UnimplementedNodeAgentServiceServer

	result *nodeagentv1.OperationResult
	err    error

	presentReq *nodeagentv1.EnsureUserPresentRequest
	absentReq  *nodeagentv1.EnsureUserAbsentRequest
	calls      int
}

func (a *fakeAgent) EnsureUserPresent(
	_ context.Context,
	req *nodeagentv1.EnsureUserPresentRequest,
) (*nodeagentv1.OperationResult, error) {
	a.calls++
	a.presentReq = req
	return a.result, a.err
}

func (a *fakeAgent) EnsureUserAbsent(
	_ context.Context,
	req *nodeagentv1.EnsureUserAbsentRequest,
) (*nodeagentv1.OperationResult, error) {
	a.calls++
	a.absentReq = req
	return a.result, a.err
}

// startAgent поднимает поддельного агента на localhost с настоящим mTLS.
//
// Агент требует и проверяет клиентский сертификат: §9 требует, чтобы он принимал
// только идентичность backend, и рукопожатие обязано проверяться в обе стороны.
func startAgent(t *testing.T, ca *testCA, agent *fakeAgent, serverSANs []string, serverURIs []string) string {
	t.Helper()

	pair := ca.issue(t, serverSANs, serverURIs, []net.IP{net.ParseIP("127.0.0.1")}, x509.ExtKeyUsageServerAuth)

	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(ca.certPEM) {
		t.Fatal("CA теста не разобрался")
	}

	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{pair},
		ClientCAs:    clientCAs,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	})))
	nodeagentv1.RegisterNodeAgentServiceServer(server, agent)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("слушатель: %v", err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	return listener.Addr().String()
}

// --- обвязка ------------------------------------------------------------------

func applied() *nodeagentv1.OperationResult {
	return &nodeagentv1.OperationResult{Status: nodeagentv1.ApplyStatus_APPLY_STATUS_APPLIED}
}

func newHarness(t *testing.T, agent *fakeAgent) (*Client, Endpoint) {
	t.Helper()

	ca := newTestCA(t)
	client, err := New(ca.issueBackendFiles(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	address := startAgent(t, ca, agent, []string{testAgentDNSName}, []string{testAgentIdentity})

	return client, Endpoint{
		NodeID:              "NL-1",
		Address:             address,
		TLSServerName:       testAgentDNSName,
		CertificateIdentity: testAgentIdentity,
	}
}

func testUser() User {
	return User{
		AccountingID: "u.abcdefghijklmnopqrst",
		ClientUUID:   crypto.NewClientUUID(uuid.MustParse("f81d4fae-7dec-11d0-a765-00a0c91e6bf6")),
		Flow:         domain.FlowXTLSRprxVision,
		EgressKey:    "de-exit",
	}
}

// --- тесты --------------------------------------------------------------------

// TestEnsureUserPresentDeliversPayload — путь целиком: рукопожатие, сверка
// идентичности и payload §9 на той стороне.
func TestEnsureUserPresentDeliversPayload(t *testing.T) {
	agent := &fakeAgent{result: applied()}
	client, endpoint := newHarness(t, agent)

	outcome := client.EnsureUserPresent(context.Background(), endpoint, "op-1", testUser())

	if outcome.Result != domain.AttemptSucceeded || outcome.Code != CodeApplied {
		t.Fatalf("исход %+v, ожидался успех", outcome)
	}

	req := agent.presentReq
	if req.GetOperationId() != "op-1" {
		t.Errorf("operation_id %q: контракт строит на нём идемпотентность (решение 38)", req.GetOperationId())
	}
	if got := req.GetUser().GetAccountingId(); got != testUser().AccountingID {
		t.Errorf("accounting_id %q", got)
	}
	if got := req.GetUser().GetCredentialUuid(); got != testUser().ClientUUID.Reveal().String() {
		t.Errorf("credential_uuid не доехал в открытом виде: %q", got)
	}
	if got := req.GetUser().GetEgressKey(); got != "de-exit" {
		t.Errorf("egress_key %q: без него агент не построит per-user rule (§9)", got)
	}
	if got := req.GetUser().GetFlow(); got != domain.FlowXTLSRprxVision {
		t.Errorf("flow %q", got)
	}
}

// TestEnsureUserAbsentSendsNoCredential — удаление матчится по accounting_id, и
// расшифровывать client_uuid ради него незачем (§9).
func TestEnsureUserAbsentSendsNoCredential(t *testing.T) {
	agent := &fakeAgent{result: applied()}
	client, endpoint := newHarness(t, agent)

	outcome := client.EnsureUserAbsent(context.Background(), endpoint, "op-2", "u.abcdefghijklmnopqrst")

	if outcome.Result != domain.AttemptSucceeded {
		t.Fatalf("исход %+v, ожидался успех", outcome)
	}
	if got := agent.absentReq.GetAccountingId(); got != "u.abcdefghijklmnopqrst" {
		t.Errorf("accounting_id %q", got)
	}
	if agent.absentReq.GetOperationId() != "op-2" {
		t.Errorf("operation_id %q", agent.absentReq.GetOperationId())
	}
}

// TestClassifyAgentStatuses — решение 37: gRPC OK и ApplyStatus в теле независимы.
func TestClassifyAgentStatuses(t *testing.T) {
	tests := []struct {
		name   string
		status nodeagentv1.ApplyStatus
		want   domain.AttemptOutcome
		code   string
		alert  bool
	}{
		{"применено", nodeagentv1.ApplyStatus_APPLY_STATUS_APPLIED, domain.AttemptSucceeded, CodeApplied, false},
		{"уже применено", nodeagentv1.ApplyStatus_APPLY_STATUS_ALREADY_APPLIED, domain.AttemptSucceeded, CodeAlreadyApplied, false},
		{"временный отказ", nodeagentv1.ApplyStatus_APPLY_STATUS_RETRYABLE_ERROR, domain.AttemptRetryable, CodeAgentRetryable, false},
		{"постоянный отказ", nodeagentv1.ApplyStatus_APPLY_STATUS_PERMANENT_ERROR, domain.AttemptPermanent, CodeAgentPermanent, false},
		{"агент не назвал исход", nodeagentv1.ApplyStatus_APPLY_STATUS_UNSPECIFIED, domain.AttemptPermanent, CodeAgentUnknown, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agent := &fakeAgent{result: &nodeagentv1.OperationResult{Status: tc.status, Message: "диагностика"}}
			client, endpoint := newHarness(t, agent)

			outcome := client.EnsureUserPresent(context.Background(), endpoint, "op", testUser())

			if outcome.Result != tc.want || outcome.Code != tc.code {
				t.Errorf("исход %+v, ожидался %s/%s", outcome, tc.want, tc.code)
			}
			if outcome.Alert != tc.alert {
				t.Errorf("alert %v, ожидался %v", outcome.Alert, tc.alert)
			}
			if outcome.Message != "диагностика" {
				t.Errorf("сообщение агента потеряно: %q", outcome.Message)
			}
		})
	}
}

// TestClassifyTransportCodes — таблица §9: что повторяется, а что терминально.
func TestClassifyTransportCodes(t *testing.T) {
	tests := []struct {
		code  codes.Code
		want  domain.AttemptOutcome
		alert bool
	}{
		{codes.Unavailable, domain.AttemptRetryable, false},
		{codes.DeadlineExceeded, domain.AttemptRetryable, false},
		{codes.Aborted, domain.AttemptRetryable, false},
		{codes.InvalidArgument, domain.AttemptPermanent, false},
		{codes.FailedPrecondition, domain.AttemptPermanent, false},
		{codes.Unauthenticated, domain.AttemptPermanent, true},
		{codes.PermissionDenied, domain.AttemptPermanent, true},
		{codes.Unimplemented, domain.AttemptPermanent, true},
		// Неопознанное повторяется, но не молча (§9).
		{codes.Internal, domain.AttemptRetryable, true},
	}

	for _, tc := range tests {
		t.Run(tc.code.String(), func(t *testing.T) {
			agent := &fakeAgent{err: status.Error(tc.code, "отказ агента")}
			client, endpoint := newHarness(t, agent)

			outcome := client.EnsureUserPresent(context.Background(), endpoint, "op", testUser())

			if outcome.Result != tc.want {
				t.Errorf("исход %s, ожидался %s", outcome.Result, tc.want)
			}
			if outcome.Alert != tc.alert {
				t.Errorf("alert %v, ожидался %v", outcome.Alert, tc.alert)
			}
		})
	}
}

// TestIdentityMismatchIsPermanent — решение 36 и главный тест этого пакета.
//
// Сертификат подписан нашим CA и имеет верное DNS-имя, поэтому рукопожатие и
// проверка цепочки проходят. Но URI SAN принадлежит другой ноде: без явной сверки
// такая нода получила бы чужие credentials.
func TestIdentityMismatchIsPermanent(t *testing.T) {
	ca := newTestCA(t)
	client, err := New(ca.issueBackendFiles(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	agent := &fakeAgent{result: applied()}
	address := startAgent(t, ca, agent,
		[]string{testAgentDNSName}, []string{"spiffe://spiritvpn/node/DE-1"})

	outcome := client.EnsureUserPresent(context.Background(), Endpoint{
		NodeID:              "NL-1",
		Address:             address,
		TLSServerName:       testAgentDNSName,
		CertificateIdentity: testAgentIdentity,
	}, "op", testUser())

	if outcome.Result != domain.AttemptPermanent {
		t.Fatalf("исход %s, ожидался PERMANENT: подмена ноды не должна ретраиться", outcome.Result)
	}
	if outcome.Code != CodeIdentityMismatch {
		t.Errorf("код %q, ожидался %q", outcome.Code, CodeIdentityMismatch)
	}
	if !outcome.Alert {
		t.Error("подмена идентичности не подняла alert (§9: security failure)")
	}
	if agent.calls != 0 {
		t.Errorf("агент получил %d вызовов: credentials уехали чужой ноде", agent.calls)
	}
}

// TestEmptyExpectedIdentityIsRejected — пустая ожидаемая идентичность совпала бы
// с любой нодой. Манифест её не пропускает, но полагаться на это здесь нельзя.
func TestEmptyExpectedIdentityIsRejected(t *testing.T) {
	agent := &fakeAgent{result: applied()}
	client, endpoint := newHarness(t, agent)
	endpoint.CertificateIdentity = ""

	outcome := client.EnsureUserPresent(context.Background(), endpoint, "op", testUser())

	if outcome.Code != CodeIdentityMismatch {
		t.Fatalf("код %q, ожидался %q", outcome.Code, CodeIdentityMismatch)
	}
	if agent.calls != 0 {
		t.Errorf("агент получил %d вызовов при пустой ожидаемой идентичности", agent.calls)
	}
}

// TestReusesConnectionPerNode — §9: на ноду переиспользуется один канал.
func TestReusesConnectionPerNode(t *testing.T) {
	agent := &fakeAgent{result: applied()}
	client, endpoint := newHarness(t, agent)

	for range 3 {
		if outcome := client.EnsureUserPresent(context.Background(), endpoint, "op", testUser()); outcome.Result != domain.AttemptSucceeded {
			t.Fatalf("исход %+v", outcome)
		}
	}

	if agent.calls != 3 {
		t.Fatalf("агент получил %d вызовов, ожидалось 3", agent.calls)
	}
	if got := client.connCount(); got != 1 {
		t.Fatalf("соединений %d, ожидалось 1", got)
	}
}

// TestEndpointChangeReplacesConnection — решение 40: смена endpoint в манифесте
// сама переводит доставку на новый адрес и закрывает прежнее соединение (§6).
func TestEndpointChangeReplacesConnection(t *testing.T) {
	ca := newTestCA(t)
	client, err := New(ca.issueBackendFiles(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	first := &fakeAgent{result: applied()}
	second := &fakeAgent{result: applied()}

	firstAddr := startAgent(t, ca, first, []string{testAgentDNSName}, []string{testAgentIdentity})
	secondAddr := startAgent(t, ca, second, []string{testAgentDNSName}, []string{testAgentIdentity})

	endpoint := Endpoint{
		NodeID:              "NL-1",
		Address:             firstAddr,
		TLSServerName:       testAgentDNSName,
		CertificateIdentity: testAgentIdentity,
	}
	client.EnsureUserPresent(context.Background(), endpoint, "op", testUser())

	endpoint.Address = secondAddr
	client.EnsureUserPresent(context.Background(), endpoint, "op", testUser())

	if first.calls != 1 || second.calls != 1 {
		t.Fatalf("вызовов: старый %d, новый %d — ожидалось по одному", first.calls, second.calls)
	}
	if got := client.connCount(); got != 1 {
		t.Fatalf("соединений %d, ожидалось 1: прежнее должно закрыться", got)
	}
}

// TestNewRejectsBrokenMaterial — процесс не должен подниматься с нечитаемым
// TLS-материалом: иначе отказ выглядел бы как «все ноды недоверены».
func TestNewRejectsBrokenMaterial(t *testing.T) {
	ca := newTestCA(t)
	cfg := ca.issueBackendFiles(t)

	t.Run("нет клиентской пары", func(t *testing.T) {
		broken := cfg
		broken.CertFile = filepath.Join(ca.dir, "нет-такого.crt")
		if _, err := New(broken); err == nil {
			t.Fatal("New принял отсутствующий сертификат")
		}
	})

	t.Run("CA без сертификатов", func(t *testing.T) {
		ca.writeFile(t, "garbage.crt", []byte("не сертификат"))

		broken := cfg
		broken.CAFile = filepath.Join(ca.dir, "garbage.crt")
		if _, err := New(broken); err == nil {
			t.Fatal("New принял CA без единого сертификата")
		}
	})
}
