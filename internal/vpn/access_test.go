package vpn

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type accessStoreStub struct {
	access  Access
	desired []DesiredUser
	err     error
	deleted []uint
}

func (s *accessStoreStub) GetOrCreate(context.Context, Identity, string, string, time.Duration) (Access, error) {
	return s.access, s.err
}

func (s *accessStoreStub) Delete(_ context.Context, id uint) error {
	s.deleted = append(s.deleted, id)
	return nil
}

func (s *accessStoreStub) ListDesired(context.Context) ([]DesiredUser, error) {
	return s.desired, s.err
}

type runtimeStub struct {
	addedUUID       string
	addedAccounting string
	removed         []string
	addErr          error
}

func (r *runtimeStub) AddUser(_ context.Context, clientUUID, accountingID string) error {
	r.addedUUID = clientUUID
	r.addedAccounting = accountingID
	return r.addErr
}

func (r *runtimeStub) RemoveUser(_ context.Context, accountingID string) error {
	r.removed = append(r.removed, accountingID)
	return nil
}

type uriBuilderStub struct {
	uri string
	err error
}

func (b uriBuilderStub) Build(string) (string, error) { return b.uri, b.err }

func TestAccessServiceIssuesNewAccess(t *testing.T) {
	expiresAt := time.Now().Add(time.Hour).UTC()
	store := &accessStoreStub{access: Access{
		ConfigID: 7, UUID: testUUID, AccountingID: "tg:42", ExpiresAt: expiresAt, Created: true,
	}}
	runtime := &runtimeStub{}
	service, err := NewAccessService(store, runtime, uriBuilderStub{uri: "vless://profile"}, time.Hour)
	require.NoError(t, err)

	profile, err := service.IssueTestAccess(context.Background(), Identity{TelegramID: 42, Username: "roman"})

	require.NoError(t, err)
	assert.Equal(t, "vless://profile", profile.URI)
	assert.Equal(t, expiresAt, profile.ExpiresAt)
	assert.Equal(t, testUUID, runtime.addedUUID)
	assert.Equal(t, "tg:42", runtime.addedAccounting)
	assert.Empty(t, store.deleted)
}

func TestAccessServiceReturnsExistingAccessWithoutDuplicateXrayUser(t *testing.T) {
	store := &accessStoreStub{access: Access{
		ConfigID: 7, UUID: testUUID, AccountingID: "tg:42", ExpiresAt: time.Now(), Created: false,
	}}
	runtime := &runtimeStub{}
	service, err := NewAccessService(store, runtime, uriBuilderStub{uri: "vless://profile"}, time.Hour)
	require.NoError(t, err)

	_, err = service.IssueTestAccess(context.Background(), Identity{TelegramID: 42})

	require.NoError(t, err)
	assert.Empty(t, runtime.addedUUID)
}

func TestAccessServiceRollsBackDesiredStateWhenXrayFails(t *testing.T) {
	store := &accessStoreStub{access: Access{
		ConfigID: 7, UUID: testUUID, AccountingID: "tg:42", ExpiresAt: time.Now(), Created: true,
	}}
	runtime := &runtimeStub{addErr: errors.New("overlay unavailable")}
	service, err := NewAccessService(store, runtime, uriBuilderStub{uri: "unused"}, time.Hour)
	require.NoError(t, err)

	_, err = service.IssueTestAccess(context.Background(), Identity{TelegramID: 42})

	assert.ErrorContains(t, err, "add Xray user")
	assert.Equal(t, []uint{7}, store.deleted)
}

func TestAccessServiceReturnsDesiredUsers(t *testing.T) {
	expected := []DesiredUser{{UUID: testUUID, Email: "tg:42", Flow: defaultVLESSFlow}}
	service, err := NewAccessService(
		&accessStoreStub{desired: expected},
		&runtimeStub{},
		uriBuilderStub{},
		time.Hour,
	)
	require.NoError(t, err)

	users, err := service.DesiredUsers(context.Background())

	require.NoError(t, err)
	assert.Equal(t, expected, users)
}

func TestVLESSURIBuilderMatchesInfrastructureContract(t *testing.T) {
	builder, err := NewVLESSURIBuilder(RealityEndpoint{
		NodeName: "entry-1", Host: "vpn.example.com", Port: 443,
		ServerName: "www.microsoft.com", PublicKey: "public-key",
		ShortID: "6ba85179e30d4fc2", Fingerprint: "chrome",
	})
	require.NoError(t, err)

	uri, err := builder.Build(testUUID)
	require.NoError(t, err)
	parsed, err := url.Parse(uri)
	require.NoError(t, err)

	assert.Equal(t, "vless", parsed.Scheme)
	assert.Equal(t, testUUID, parsed.User.Username())
	assert.Equal(t, "vpn.example.com:443", parsed.Host)
	assert.Equal(t, "none", parsed.Query().Get("encryption"))
	assert.Equal(t, defaultVLESSFlow, parsed.Query().Get("flow"))
	assert.Equal(t, "reality", parsed.Query().Get("security"))
	assert.Equal(t, "www.microsoft.com", parsed.Query().Get("sni"))
	assert.Equal(t, "chrome", parsed.Query().Get("fp"))
	assert.Equal(t, "public-key", parsed.Query().Get("pbk"))
	assert.Equal(t, "6ba85179e30d4fc2", parsed.Query().Get("sid"))
	assert.Equal(t, "tcp", parsed.Query().Get("type"))
	assert.Equal(t, "SpiritVPN entry-1", parsed.Fragment)
}

func TestVLESSURIBuilderRejectsPrivateConfigurationGaps(t *testing.T) {
	_, err := NewVLESSURIBuilder(RealityEndpoint{NodeName: "entry-1", Host: "vpn.example.com", Port: 443})
	assert.ErrorContains(t, err, "reality server name and public key")
}
