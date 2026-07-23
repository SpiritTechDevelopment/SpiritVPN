package vpn

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// NodeEndpoint содержит публичные Reality-параметры ноды и адрес её Xray API.
// Значение формируется из версионированного манифеста репозитория Infrastructure.
type NodeEndpoint struct {
	Reality RealityEndpoint
	APIHost string
	APIPort int
}

// endpointManifest описывает версионированный набор доступных entry-нод.
type endpointManifest struct {
	SchemaVersion int                              `json:"schema_version"`
	Entries       map[string]endpointManifestEntry `json:"entries"`
}

// endpointManifestEntry соответствует одной entry-ноде в инфраструктурном манифесте.
type endpointManifestEntry struct {
	Address         string `json:"address"`
	Port            int    `json:"port"`
	ServerName      string `json:"server_name"`
	ShortID         string `json:"short_id"`
	RealityPassword string `json:"reality_password"`
	Fingerprint     string `json:"fingerprint"`
	Flow            string `json:"flow"`
	APIHost         string `json:"api_host"`
	APIPort         int    `json:"api_port"`
	DefaultExitTag  string `json:"default_exit_tag"`
	XrayImage       string `json:"xray_image"`
}

// LoadNodeEndpoint загружает и валидирует параметры указанной entry-ноды
// из защищённого client-endpoints.json, сформированного Infrastructure.
func LoadNodeEndpoint(path, nodeName string) (NodeEndpoint, error) {
	file, err := os.Open(path)
	if err != nil {
		return NodeEndpoint{}, fmt.Errorf("open endpoint manifest: %w", err)
	}
	defer func() { _ = file.Close() }()

	var manifest endpointManifest
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return NodeEndpoint{}, fmt.Errorf("decode endpoint manifest: %w", err)
	}
	if manifest.SchemaVersion != 1 {
		return NodeEndpoint{}, fmt.Errorf("unsupported endpoint manifest schema %d", manifest.SchemaVersion)
	}
	entry, ok := manifest.Entries[nodeName]
	if !ok {
		return NodeEndpoint{}, fmt.Errorf("node %q is absent from endpoint manifest", nodeName)
	}
	if entry.Flow != "" && entry.Flow != defaultVLESSFlow {
		return NodeEndpoint{}, fmt.Errorf("unsupported VLESS flow %q", entry.Flow)
	}
	if entry.APIHost == "" || entry.APIPort < 1 || entry.APIPort > 65535 {
		return NodeEndpoint{}, errors.New("endpoint manifest contains invalid Xray API address")
	}

	result := NodeEndpoint{
		Reality: RealityEndpoint{
			NodeName: nodeName, Host: entry.Address, Port: entry.Port,
			ServerName: entry.ServerName, PublicKey: entry.RealityPassword,
			ShortID: entry.ShortID, Fingerprint: entry.Fingerprint,
		},
		APIHost: entry.APIHost,
		APIPort: entry.APIPort,
	}
	if err := result.Reality.Validate(); err != nil {
		return NodeEndpoint{}, fmt.Errorf("invalid Reality endpoint in manifest: %w", err)
	}
	return result, nil
}
