package vpn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const defaultVLESSFlow = "xtls-rprx-vision"

// Identity содержит данные внешнего пользователя, необходимые для выдачи VPN доступа.
type Identity struct {
	TelegramID int64
	Username   string
}

// Access описывает сохранённое целевое состояние VPN доступа пользователя.
type Access struct {
	ConfigID     uint
	UUID         string
	AccountingID string
	ExpiresAt    time.Time
	Created      bool
}

// ClientProfile содержит клиентские параметры подключения, безопасные для передачи пользователю.
type ClientProfile struct {
	URI       string    `json:"uri"`
	UUID      string    `json:"uuid"`
	ExpiresAt time.Time `json:"expires_at"`
}

// DesiredUser описывает пользователя, который должен присутствовать
// в runtime-состоянии Xray.
type DesiredUser struct {
	UUID  string `json:"uuid"`
	Email string `json:"email"`
	Flow  string `json:"flow"`
}

// AccessStore определяет хранилище целевого состояния VPN-доступов.
type AccessStore interface {
	// GetOrCreate возвращает существующий доступ либо сохраняет новый.
	GetOrCreate(ctx context.Context, identity Identity, clientUUID, accountingID string, ttl time.Duration) (Access, error)
	// Delete удаляет ранее сохранённый доступ по идентификатору конфигурации.
	Delete(ctx context.Context, configID uint) error
	// ListDesired возвращает активных пользователей, назначенных текущей VPN-ноде.
	ListDesired(ctx context.Context) ([]DesiredUser, error)
}

// RuntimeUserManager управляет набором пользователей в runtime-состоянии Xray.
type RuntimeUserManager interface {
	// AddUser добавляет VLESS-пользователя в runtime-состояние VPN-ядра.
	AddUser(ctx context.Context, clientUUID, accountingID string) error
	// RemoveUser удаляет пользователя из runtime-состояния по ID аккаунта.
	RemoveUser(ctx context.Context, accountingID string) error
}

// URIBuilder формирует для передачи клиентскую ссылку подключения.
type URIBuilder interface {
	// Build формирует клиентскую ссылку подключения для UUID пользователя.
	Build(clientUUID string) (string, error)
}

// AccessService координирует постоянное состояние доступа, runtime-состояние Xray
// и формирование клиентского профиля.
type AccessService struct {
	store   AccessStore
	runtime RuntimeUserManager
	builder URIBuilder
	ttl     time.Duration
}

// NewAccessService создаёт сервис выдачи тестового VPN-доступа.
// Все зависимости обязательны, а срок действия доступа должен быть положительным.
func NewAccessService(store AccessStore, runtime RuntimeUserManager, builder URIBuilder, ttl time.Duration) (*AccessService, error) {
	if store == nil || runtime == nil || builder == nil {
		return nil, errors.New("access service dependencies must not be nil")
	}
	if ttl <= 0 {
		return nil, errors.New("test access TTL must be positive")
	}
	return &AccessService{store: store, runtime: runtime, builder: builder, ttl: ttl}, nil
}

// IssueTestAccess возвращает существующий профиль либо создаёт новый доступ
// в постоянном хранилище и добавляет пользователя в runtime-состояние Xray.
func (s *AccessService) IssueTestAccess(ctx context.Context, identity Identity) (ClientProfile, error) {
	if identity.TelegramID <= 0 {
		return ClientProfile{}, errors.New("telegram user ID must be positive")
	}

	accountingID := "tg:" + strconv.FormatInt(identity.TelegramID, 10)
	access, err := s.store.GetOrCreate(ctx, identity, uuid.NewString(), accountingID, s.ttl)
	if err != nil {
		return ClientProfile{}, fmt.Errorf("persist VPN access: %w", err)
	}

	if access.Created {
		if err := s.runtime.AddUser(ctx, access.UUID, access.AccountingID); err != nil {
			cleanupErr := s.store.Delete(context.WithoutCancel(ctx), access.ConfigID)
			if cleanupErr != nil {
				return ClientProfile{}, fmt.Errorf("add Xray user: %w (rollback failed: %v)", err, cleanupErr)
			}
			return ClientProfile{}, fmt.Errorf("add Xray user: %w", err)
		}
	}

	uri, err := s.builder.Build(access.UUID)
	if err != nil {
		if access.Created {
			_ = s.runtime.RemoveUser(context.WithoutCancel(ctx), access.AccountingID)
			_ = s.store.Delete(context.WithoutCancel(ctx), access.ConfigID)
		}
		return ClientProfile{}, fmt.Errorf("build VLESS profile: %w", err)
	}

	return ClientProfile{URI: uri, UUID: access.UUID, ExpiresAt: access.ExpiresAt}, nil
}

// DesiredUsers возвращает актуальный снимок пользователей, которые должны
// присутствовать в Xray. Постоянное хранилище остаётся источником истины.
func (s *AccessService) DesiredUsers(ctx context.Context) ([]DesiredUser, error) {
	users, err := s.store.ListDesired(ctx)
	if err != nil {
		return nil, fmt.Errorf("list desired VPN users: %w", err)
	}
	return users, nil
}

// RealityEndpoint содержит публичные параметры Reality entry-ноды,
// необходимые для формирования клиентского VLESS-профиля.
type RealityEndpoint struct {
	NodeName    string
	Host        string
	Port        int
	ServerName  string
	PublicKey   string
	ShortID     string
	Fingerprint string
}

// Validate проверяет полноту и допустимость публичных параметров Reality endpoint.
func (e RealityEndpoint) Validate() error {
	if strings.TrimSpace(e.NodeName) == "" || strings.TrimSpace(e.Host) == "" {
		return errors.New("VPN node name and public host are required")
	}
	if e.Port < 1 || e.Port > 65535 {
		return errors.New("VPN public port is invalid")
	}
	if strings.TrimSpace(e.ServerName) == "" || strings.TrimSpace(e.PublicKey) == "" {
		return errors.New("reality server name and public key are required")
	}
	if strings.TrimSpace(e.Fingerprint) == "" {
		return errors.New("reality fingerprint is required")
	}
	return nil
}

// VLESSURIBuilder формирует клиентские VLESS-ссылки по параметрам Reality endpoint.
type VLESSURIBuilder struct {
	endpoint RealityEndpoint
}

// NewVLESSURIBuilder создаёт построитель VLESS-ссылок и валидирует endpoint.
func NewVLESSURIBuilder(endpoint RealityEndpoint) (*VLESSURIBuilder, error) {
	if err := endpoint.Validate(); err != nil {
		return nil, err
	}
	return &VLESSURIBuilder{endpoint: endpoint}, nil
}

// Build формирует VLESS-ссылку для переданного UUID клиента.
func (b *VLESSURIBuilder) Build(clientUUID string) (string, error) {
	if _, err := uuid.Parse(clientUUID); err != nil {
		return "", fmt.Errorf("invalid client UUID: %w", err)
	}

	query := url.Values{
		"encryption": {"none"},
		"flow":       {defaultVLESSFlow},
		"security":   {"reality"},
		"sni":        {b.endpoint.ServerName},
		"fp":         {b.endpoint.Fingerprint},
		"pbk":        {b.endpoint.PublicKey},
		"sid":        {b.endpoint.ShortID},
		"type":       {"tcp"},
	}

	profile := &url.URL{
		Scheme:   "vless",
		User:     url.User(clientUUID),
		Host:     net.JoinHostPort(b.endpoint.Host, strconv.Itoa(b.endpoint.Port)),
		RawQuery: query.Encode(),
		Fragment: "SpiritVPN " + b.endpoint.NodeName,
	}
	return profile.String(), nil
}
