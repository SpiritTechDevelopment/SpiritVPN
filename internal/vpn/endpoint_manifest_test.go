package vpn

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadNodeEndpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client-endpoints.json")
	content := `{
  "schema_version": 1,
  "entries": {
    "entry-1": {
      "address": "vpn.example.com",
      "port": 443,
      "server_name": "www.microsoft.com",
      "short_id": "6ba85179e30d4fc2",
      "reality_password": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "fingerprint": "chrome",
      "flow": "xtls-rprx-vision",
      "api_host": "10.20.0.11",
      "api_port": 10085
    }
  }
}`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	endpoint, err := LoadNodeEndpoint(path, "entry-1")

	require.NoError(t, err)
	assert.Equal(t, "vpn.example.com", endpoint.Reality.Host)
	assert.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", endpoint.Reality.PublicKey)
	assert.Equal(t, "10.20.0.11", endpoint.APIHost)
	assert.Equal(t, 10085, endpoint.APIPort)
}

func TestLoadNodeEndpointRejectsUnknownNode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client-endpoints.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"schema_version":1,"entries":{}}`), 0o600))

	_, err := LoadNodeEndpoint(path, "entry-1")

	assert.ErrorContains(t, err, "is absent")
}
