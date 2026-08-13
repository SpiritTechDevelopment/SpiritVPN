// Package nodeagent — клиент node-agent, единственная исходящая сетевая
// поверхность backend.
//
// Backend здесь клиент, а не сервер: он вызывает EnsureUserPresent/Absent на
// агенте входной ноды поверх mTLS в management WireGuard overlay. Сам Xray
// backend не трогает никогда — команды транслирует агент.
//
// Контракт (proto/spiritvpn/nodeagent/v1) принадлежит инфраструктуре и живёт в
// отдельном репозитории; здесь лежит только вендорная копия, и менять её нельзя —
// изменения запрашиваются у владельца.
package nodeagent

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync"
)

// ErrIdentityMismatch — предъявленный агентом сертификат прошёл проверку цепочки,
// но его идентичность не совпала с certificate_identity из манифеста.
//
// Это security failure: такое не хот-лупится и порождает alert.
// Отдельный сентинел нужен, чтобы отличить подмену ноды от обычной недоступности,
// которая ретраится.
var ErrIdentityMismatch = errors.New("nodeagent: идентичность агента не совпала с манифестом")

// ErrEndpointIncomplete — agent_config ноды не даёт достаточно данных для вызова.
//
// Манифест такое не пропускает: endpoint, tls_server_name и
// certificate_identity проверяются до записи. Наступить на это можно только через
// испорченный jsonb колонки. Ошибка своя, а не общая transport, потому что исход
// у неё особый: retryable с alert. Permanent здесь был бы вечным —
// починка манифеста не меняет desired_version access и новой операции не
// порождает, так что ретраить было бы уже некому.
var ErrEndpointIncomplete = errors.New("nodeagent: неполный agent_config ноды")

// identityCheck хранит результат последней сверки идентичности на соединении.
//
// Нужен потому, что gRPC не сохраняет цепочку ошибок рукопожатия: наружу
// приезжает status с кодом Unavailable и текстом внутри, а errors.Is до
// ErrIdentityMismatch не достаёт. Разбирать текст было бы гаданием — он
// принадлежит чужой библиотеке и меняется вместе с её версией.
//
// Пишется на каждое рукопожатие, включая успешное: иначе исправленный на ноде
// сертификат навсегда оставался бы «подменой».
type identityCheck struct {
	mu  sync.Mutex
	err error
}

func (c *identityCheck) record(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.err = err
}

// failure возвращает ошибку последней сверки, если она провалилась.
func (c *identityCheck) failure() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.err
}

// tlsFiles — клиентская пара backend и CA, которым подписаны сертификаты агентов.
type tlsFiles struct {
	CertFile string
	KeyFile  string
	CAFile   string
}

// loadTLSMaterial читает пару и CA один раз при старте.
//
// Пара отдельная от серверной: здесь backend предъявляет себя
// агенту, там принимает product-сервис. Общий сертификат смешал бы две роли, и
// компрометация одной автоматически означала бы компрометацию другой.
func loadTLSMaterial(files tlsFiles) (tls.Certificate, *x509.CertPool, error) {
	cert, err := tls.LoadX509KeyPair(files.CertFile, files.KeyFile)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("nodeagent: клиентская пара TLS: %w", err)
	}

	caPEM, err := os.ReadFile(files.CAFile)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("nodeagent: CA агентов: %w", err)
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		// AppendCertsFromPEM молча игнорирует мусор. Без проверки процесс поднялся
		// бы с пустым пулом и не смог достучаться ни до одной ноды — с
		// диагностикой «сертификат не доверен» вместо «CA не прочитан».
		return tls.Certificate{}, nil, fmt.Errorf("nodeagent: CA агентов: %s не содержит ни одного сертификата", files.CAFile)
	}

	return cert, roots, nil
}

// tlsConfigFor собирает конфигурацию под одну ноду.
//
// ServerName берётся из манифеста и задаёт как SNI, так и имя, по которому
// проверяется сертификат. Сверх этого проверяется идентичность:
// валидная цепочка отвечает только на вопрос «сертификат выпущен нашим CA», а им
// подписаны сертификаты всех нод. Без явной сверки любая нода могла бы выдать
// себя за любую другую и получить чужие credentials.
func tlsConfigFor(
	cert tls.Certificate,
	roots *x509.CertPool,
	endpoint Endpoint,
	check *identityCheck,
) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      roots,
		ServerName:   endpoint.TLSServerName,

		// В TLS 1.3 сертификаты обеих сторон передаются уже зашифрованными.
		MinVersion: tls.VersionTLS13,

		VerifyPeerCertificate: verifyAgentIdentity(endpoint.CertificateIdentity, check),
	}
}

// verifyAgentIdentity сверяет SAN проверенной цепочки с ожидаемой идентичностью.
//
// Вызывается уже после штатной проверки цепочки и имени, поэтому verifiedChains
// непуст, а leaf доверен. Читаются DNS SAN и URI SAN — те же поля, что и на
// серверной стороне; CN не используется.
func verifyAgentIdentity(expected string, check *identityCheck) func([][]byte, [][]*x509.Certificate) error {
	return func(_ [][]byte, verifiedChains [][]*x509.Certificate) error {
		err := matchAgentIdentity(expected, verifiedChains)
		check.record(err)
		return err
	}
}

// matchAgentIdentity — собственно сверка.
func matchAgentIdentity(expected string, verifiedChains [][]*x509.Certificate) error {
	if expected == "" {
		// Пустая ожидаемая идентичность совпала бы с чем угодно. Манифест её
		// не пропускает — certificate_identity обязателен, — но полагаться на
		// это здесь нельзя: цена ошибки — принятая чужая нода.
		return fmt.Errorf("%w: манифест не задал ожидаемую идентичность", ErrIdentityMismatch)
	}

	for _, chain := range verifiedChains {
		if len(chain) == 0 {
			continue
		}
		leaf := chain[0]

		for _, name := range leaf.DNSNames {
			if name == expected {
				return nil
			}
		}
		for _, uri := range leaf.URIs {
			if uri.String() == expected {
				return nil
			}
		}
	}

	return fmt.Errorf("%w: ожидалась %q", ErrIdentityMismatch, expected)
}
