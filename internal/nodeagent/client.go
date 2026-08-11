package nodeagent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	nodeagentv1 "github.com/RomanRyabinkin/SpiritVPN/internal/gen/spiritvpn/nodeagent/v1"
)

// DefaultCallTimeout — deadline обычной операции (§9).
const DefaultCallTimeout = 5 * time.Second

// Endpoint — как достучаться до агента одной ноды. Всё берётся из
// vpn_nodes.agent_config, то есть из манифеста (§6).
type Endpoint struct {
	NodeID              string
	Address             string
	TLSServerName       string
	CertificateIdentity string
}

// key — ключ кеша каналов (решение 40).
//
// В него входит всё, что задаёт соединение. Смена endpoint или TLS-имени в
// манифесте сама даёт новый ключ, поэтому §6 «направляет следующие agent calls на
// новый endpoint» выполняется без отдельной инвалидации: старый канал просто
// перестаёт запрашиваться и закрывается.
func (e Endpoint) key() string {
	return e.NodeID + "|" + e.Address + "|" + e.TLSServerName + "|" + e.CertificateIdentity
}

// validate отсекает endpoint, по которому вызов заведомо не состоится.
//
// Пустой CertificateIdentity особенно опасен: сверка идентичности его отвергнет
// как подмену (§9, security failure), то есть испорченная колонка выглядела бы в
// логах атакой. Здесь она называется своим именем.
func (e Endpoint) validate() error {
	switch {
	case e.NodeID == "":
		return fmt.Errorf("%w: пустой node_id", ErrEndpointIncomplete)
	case e.Address == "":
		return fmt.Errorf("%w: нода %s не имеет endpoint", ErrEndpointIncomplete, e.NodeID)
	case e.TLSServerName == "":
		return fmt.Errorf("%w: нода %s не имеет tls_server_name", ErrEndpointIncomplete, e.NodeID)
	case e.CertificateIdentity == "":
		return fmt.Errorf("%w: нода %s не имеет certificate_identity", ErrEndpointIncomplete, e.NodeID)
	default:
		return nil
	}
}

// User — payload EnsureUserPresent (§9).
//
// ClientUUID типизирован crypto.ClientUUID, а не строкой: открытое значение
// живёт только внутри вызова и не должно попадать в лог по дороге.
type User struct {
	AccountingID string
	ClientUUID   crypto.ClientUUID
	Flow         string
	EgressKey    string
}

// Config — настройки клиента.
type Config struct {
	CertFile string
	KeyFile  string
	CAFile   string
	// CallTimeout ограничивает один mutating RPC; ноль означает DefaultCallTimeout.
	CallTimeout time.Duration
	// PullTimeout ограничивает GetNodeState; ноль означает DefaultPullTimeout.
	PullTimeout time.Duration
}

// Client вызывает агентов, переиспользуя по одному соединению на ноду (§9).
type Client struct {
	cert        tls.Certificate
	roots       *x509.CertPool
	callTimeout time.Duration
	pullTimeout time.Duration

	mu    sync.Mutex
	conns map[string]*agentConn
}

// agentConn — соединение с нодой вместе с результатом сверки её идентичности.
type agentConn struct {
	conn     *grpc.ClientConn
	identity *identityCheck
}

// New собирает клиента и читает TLS-материал с диска.
func New(cfg Config) (*Client, error) {
	cert, roots, err := loadTLSMaterial(tlsFiles{
		CertFile: cfg.CertFile,
		KeyFile:  cfg.KeyFile,
		CAFile:   cfg.CAFile,
	})
	if err != nil {
		return nil, err
	}

	timeout := cfg.CallTimeout
	if timeout <= 0 {
		timeout = DefaultCallTimeout
	}
	pullTimeout := cfg.PullTimeout
	if pullTimeout <= 0 {
		pullTimeout = DefaultPullTimeout
	}

	return &Client{
		cert:        cert,
		roots:       roots,
		callTimeout: timeout,
		pullTimeout: pullTimeout,
		conns:       make(map[string]*agentConn),
	}, nil
}

// EnsureUserPresent ставит юзера на ноду (§9).
//
// operation_id уходит полем запроса, а не только trace-метадатой, как допускает
// §9: вендорный контракт объявляет его обязательным и строит на нём собственную
// идемпотентность («повтор с другим payload — permanent error»). Контракт
// принадлежит другому репозиторию, и здесь он побеждает текст спеки (решение 38).
func (c *Client) EnsureUserPresent(
	ctx context.Context,
	endpoint Endpoint,
	operationID string,
	user User,
) Outcome {
	agent, err := c.connFor(endpoint)
	if err != nil {
		return classifyTransport(err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.callTimeout)
	defer cancel()

	result, err := nodeagentv1.NewNodeAgentServiceClient(agent.conn).EnsureUserPresent(ctx, &nodeagentv1.EnsureUserPresentRequest{
		OperationId: operationID,
		User: &nodeagentv1.User{
			AccountingId: user.AccountingID,
			// Единственная точка, где открытый client_uuid покидает тип и уходит
			// на провод. Дальше он живёт только в памяти агента (§7).
			CredentialUuid: user.ClientUUID.Reveal().String(),
			Flow:           user.Flow,
			EgressKey:      user.EgressKey,
		},
	})

	return classifyWithIdentity(agent, result, err)
}

// EnsureUserAbsent снимает юзера с ноды (§9).
//
// credential_uuid здесь не передаётся вовсе: удаление матчится по accounting_id
// (Xray email), и расшифровывать credential ради него незачем.
func (c *Client) EnsureUserAbsent(
	ctx context.Context,
	endpoint Endpoint,
	operationID string,
	accountingID string,
) Outcome {
	agent, err := c.connFor(endpoint)
	if err != nil {
		return classifyTransport(err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.callTimeout)
	defer cancel()

	result, err := nodeagentv1.NewNodeAgentServiceClient(agent.conn).EnsureUserAbsent(ctx, &nodeagentv1.EnsureUserAbsentRequest{
		OperationId:  operationID,
		AccountingId: accountingID,
	})

	return classifyWithIdentity(agent, result, err)
}

// classifyWithIdentity уточняет исход провалившегося вызова.
//
// Подмена идентичности приезжает от gRPC обычным Unavailable, поэтому одного
// кода мало: настоящую причину знает только сверка на рукопожатии. Без этой
// уточнялки security failure ретраился бы вечно как недоступность.
func classifyWithIdentity(agent *agentConn, result *nodeagentv1.OperationResult, err error) Outcome {
	if err != nil {
		if identityErr := agent.identity.failure(); identityErr != nil {
			return classifyTransport(identityErr)
		}
	}
	return classify(result, err)
}

// clientFor возвращает соединение с нодой, создавая его при первом обращении.
//
// grpc.NewClient не подключается немедленно, поэтому создание канала дёшево, а
// рукопожатие (и проверка идентичности) происходит на первом RPC — и приезжает
// сюда обычной transport-ошибкой, которую классифицирует classifyTransport.
func (c *Client) connFor(endpoint Endpoint) (*agentConn, error) {
	if err := endpoint.validate(); err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	key := endpoint.key()
	if agent, ok := c.conns[key]; ok {
		return agent, nil
	}

	// Соединения нод, чей endpoint сменился, больше не понадобятся: ключ у них
	// другой, и запрашивать их никто не будет. Закрываем сразу, чтобы кеш не рос
	// на каждую правку манифеста.
	c.closeStaleLocked(endpoint.NodeID)

	check := &identityCheck{}
	conn, err := grpc.NewClient(
		endpoint.Address,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfigFor(c.cert, c.roots, endpoint, check))),
	)
	if err != nil {
		return nil, err
	}

	agent := &agentConn{conn: conn, identity: check}
	c.conns[key] = agent
	return agent, nil
}

// closeStaleLocked закрывает прежние соединения той же ноды. Вызывается под mu.
func (c *Client) closeStaleLocked(nodeID string) {
	prefix := nodeID + "|"
	for key, agent := range c.conns {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			_ = agent.conn.Close()
			delete(c.conns, key)
		}
	}
}

// Close закрывает все соединения. Владелец клиента — composition root.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for key, agent := range c.conns {
		_ = agent.conn.Close()
		delete(c.conns, key)
	}
	return nil
}

// connCount — число открытых соединений. Нужен тестам кеша каналов: снаружи
// пакета он не виден, а проверять переиспользование иначе нечем.
func (c *Client) connCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.conns)
}
