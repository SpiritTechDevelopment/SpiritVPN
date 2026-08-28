package domain

import (
	"errors"
	"strings"
	"testing"
)

// Фикстуры манифеста. Идентификаторы намеренно ASCII: node_id уезжает в DNS SAN
// и TLS server name, а X.509 требует IA5String — кириллица там невозможна
// физически, но в структуре x509.Certificate такая ошибка проходит молча.

func manifestNode(id string) ManifestNode {
	return ManifestNode{
		NodeID: NodeID(id),
		Agent: NodeAgent{
			Endpoint:            "10.0.0.11:9443",
			TLSServerName:       strings.ToLower(id) + ".agent.internal",
			CertificateIdentity: "spiffe://spiritvpn/node/" + id,
		},
		Public: NodePublic{
			Address:          strings.ToLower(id) + ".example.com",
			Port:             443,
			RealityPublicKey: "pub-key",
			ServerName:       "www.example.org",
			ShortID:          "ab12",
			Fingerprint:      "chrome",
			Transport:        TransportTCP,
			Flow:             FlowXTLSRprxVision,
			DisplayName:      id,
		},
	}
}

// validSnapshot — две ноды, один fleet, одна связь.
func validSnapshot() ManifestSnapshot {
	return ManifestSnapshot{
		SchemaVersion: ManifestSchemaVersion,
		Revision:      42,
		Nodes:         []ManifestNode{manifestNode("NL-1"), manifestNode("DE-1")},
		Fleets: []ManifestFleet{{
			FleetID: 10,
			NodeIDs: []NodeID{"NL-1", "DE-1"},
			Bridges: []ManifestBridge{{
				RoutingKey:  "nl-1.to-de-1",
				EntryNodeID: "NL-1",
				ExitNodeID:  "DE-1",
				EgressTag:   "de-exit",
				DisplayName: "Netherlands via Germany",
			}},
		}},
	}
}

func TestValidateManifestAcceptsSpecExample(t *testing.T) {
	if err := ValidateManifest(validSnapshot()); err != nil {
		t.Fatalf("валидный пример отвергнут: %v", err)
	}
}

func TestValidateManifestAcceptsV1TCP(t *testing.T) {
	snapshot := validSnapshot()
	snapshot.SchemaVersion = ManifestSchemaVersionV1

	if err := ValidateManifest(snapshot); err != nil {
		t.Fatalf("валидный TCP-манифест v1 отвергнут: %v", err)
	}
}

func TestValidateManifestAcceptsXHTTP(t *testing.T) {
	for _, mode := range []string{"auto", "packet-up", "stream-up", "stream-one"} {
		t.Run(mode, func(t *testing.T) {
			snapshot := validSnapshot()
			snapshot.Nodes[0].Public.Transport = TransportXHTTP
			snapshot.Nodes[0].Public.XHTTP = &XHTTPConfig{Path: "/api/v1/connect", Mode: mode}

			if err := ValidateManifest(snapshot); err != nil {
				t.Fatalf("валидный XHTTP-манифест отвергнут: %v", err)
			}
		})
	}
}

func TestValidateManifestRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ManifestSnapshot)
		want   error
	}{
		{
			name:   "чужая schema_version",
			mutate: func(s *ManifestSnapshot) { s.SchemaVersion = 3 },
			want:   ErrManifestSchemaVersion,
		},
		{
			name:   "нулевая revision",
			mutate: func(s *ManifestSnapshot) { s.Revision = 0 },
			want:   ErrManifestRevisionInvalid,
		},

		// --- уникальность внутри снапшота ---
		{
			name:   "повтор node_id",
			mutate: func(s *ManifestSnapshot) { s.Nodes = append(s.Nodes, manifestNode("NL-1")) },
			want:   ErrManifestDuplicate,
		},
		{
			name: "повтор vpn_fleet_id",
			mutate: func(s *ManifestSnapshot) {
				s.Fleets = append(s.Fleets, ManifestFleet{FleetID: 10, NodeIDs: []NodeID{"NL-1"}})
			},
			want: ErrManifestDuplicate,
		},
		{
			name: "повтор routing_key внутри fleet",
			mutate: func(s *ManifestSnapshot) {
				bridge := s.Fleets[0].Bridges[0]
				bridge.EntryNodeID, bridge.ExitNodeID = "DE-1", "NL-1"
				s.Fleets[0].Bridges = append(s.Fleets[0].Bridges, bridge)
			},
			want: ErrManifestDuplicate,
		},
		{
			// Пара (entry, exit) уникальна внутри fleet.
			name: "две связи с одной парой",
			mutate: func(s *ManifestSnapshot) {
				s.Fleets[0].Bridges = append(s.Fleets[0].Bridges, ManifestBridge{
					RoutingKey:  "nl-1.to-de-1.copy",
					EntryNodeID: "NL-1",
					ExitNodeID:  "DE-1",
					EgressTag:   "de-exit",
				})
			},
			want: ErrManifestDuplicate,
		},
		{
			name:   "нода указана во fleet дважды",
			mutate: func(s *ManifestSnapshot) { s.Fleets[0].NodeIDs = []NodeID{"NL-1", "NL-1"} },
			want:   ErrManifestDuplicate,
		},

		// --- ссылочная целостность ---
		{
			name:   "fleet ссылается на неизвестную ноду",
			mutate: func(s *ManifestSnapshot) { s.Fleets[0].NodeIDs = append(s.Fleets[0].NodeIDs, "FR-9") },
			want:   ErrManifestUnknownNode,
		},
		{
			name: "связь ссылается на неизвестную ноду",
			mutate: func(s *ManifestSnapshot) {
				s.Nodes = append(s.Nodes, manifestNode("FR-9"))
				s.Fleets[0].Bridges[0].ExitNodeID = "XX-1"
			},
			want: ErrManifestUnknownNode,
		},
		{
			name: "конец связи не входит во fleet",
			mutate: func(s *ManifestSnapshot) {
				s.Nodes = append(s.Nodes, manifestNode("FR-9"))
				s.Fleets[0].Bridges[0].ExitNodeID = "FR-9"
			},
			want: ErrManifestBridgeInvalid,
		},
		{
			name:   "entry совпадает с exit",
			mutate: func(s *ManifestSnapshot) { s.Fleets[0].Bridges[0].ExitNodeID = "NL-1" },
			want:   ErrManifestBridgeInvalid,
		},
		{
			name:   "пустой egress_tag",
			mutate: func(s *ManifestSnapshot) { s.Fleets[0].Bridges[0].EgressTag = "" },
			want:   ErrManifestBridgeInvalid,
		},
		{
			name:   "пустой routing_key",
			mutate: func(s *ManifestSnapshot) { s.Fleets[0].Bridges[0].RoutingKey = "" },
			want:   ErrManifestBridgeInvalid,
		},

		// --- параметры ноды ---
		{
			name:   "пустой node_id",
			mutate: func(s *ManifestSnapshot) { s.Nodes[0].NodeID = "" },
			want:   ErrManifestNodeInvalid,
		},
		{
			name:   "endpoint без порта",
			mutate: func(s *ManifestSnapshot) { s.Nodes[0].Agent.Endpoint = "10.0.0.11" },
			want:   ErrManifestNodeInvalid,
		},
		{
			name:   "нечисловой порт endpoint",
			mutate: func(s *ManifestSnapshot) { s.Nodes[0].Agent.Endpoint = "10.0.0.11:грпц" },
			want:   ErrManifestNodeInvalid,
		},
		{
			name:   "пустой tls_server_name",
			mutate: func(s *ManifestSnapshot) { s.Nodes[0].Agent.TLSServerName = "" },
			want:   ErrManifestNodeInvalid,
		},
		{
			name:   "пустой certificate_identity",
			mutate: func(s *ManifestSnapshot) { s.Nodes[0].Agent.CertificateIdentity = "" },
			want:   ErrManifestNodeInvalid,
		},
		{
			name:   "пустой address",
			mutate: func(s *ManifestSnapshot) { s.Nodes[0].Public.Address = "" },
			want:   ErrManifestNodeInvalid,
		},
		{
			name:   "порт вне диапазона",
			mutate: func(s *ManifestSnapshot) { s.Nodes[0].Public.Port = 65536 },
			want:   ErrManifestNodeInvalid,
		},
		{
			name:   "пустой reality_public_key",
			mutate: func(s *ManifestSnapshot) { s.Nodes[0].Public.RealityPublicKey = "" },
			want:   ErrManifestNodeInvalid,
		},
		{
			name:   "пустой server_name",
			mutate: func(s *ManifestSnapshot) { s.Nodes[0].Public.ServerName = "" },
			want:   ErrManifestNodeInvalid,
		},
		{
			name:   "пустой fingerprint",
			mutate: func(s *ManifestSnapshot) { s.Nodes[0].Public.Fingerprint = "" },
			want:   ErrManifestNodeInvalid,
		},
		{
			name:   "fingerprint с недопустимым символом",
			mutate: func(s *ManifestSnapshot) { s.Nodes[0].Public.Fingerprint = "chrome/119" },
			want:   ErrManifestNodeInvalid,
		},
		{
			name:   "fingerprint длиннее 64",
			mutate: func(s *ManifestSnapshot) { s.Nodes[0].Public.Fingerprint = strings.Repeat("a", 65) },
			want:   ErrManifestNodeInvalid,
		},
		{
			name:   "неподдерживаемый transport",
			mutate: func(s *ManifestSnapshot) { s.Nodes[0].Public.Transport = "ws" },
			want:   ErrManifestNodeInvalid,
		},
		{
			name: "xhttp без настроек",
			mutate: func(s *ManifestSnapshot) {
				s.Nodes[0].Public.Transport = TransportXHTTP
			},
			want: ErrManifestNodeInvalid,
		},
		{
			name: "tcp с настройками xhttp",
			mutate: func(s *ManifestSnapshot) {
				s.Nodes[0].Public.XHTTP = &XHTTPConfig{Path: "/connect", Mode: "auto"}
			},
			want: ErrManifestNodeInvalid,
		},
		{
			name: "xhttp path без начального слеша",
			mutate: func(s *ManifestSnapshot) {
				s.Nodes[0].Public.Transport = TransportXHTTP
				s.Nodes[0].Public.XHTTP = &XHTTPConfig{Path: "connect", Mode: "auto"}
			},
			want: ErrManifestNodeInvalid,
		},
		{
			name: "xhttp path с query",
			mutate: func(s *ManifestSnapshot) {
				s.Nodes[0].Public.Transport = TransportXHTTP
				s.Nodes[0].Public.XHTTP = &XHTTPConfig{Path: "/connect?token=1", Mode: "auto"}
			},
			want: ErrManifestNodeInvalid,
		},
		{
			name: "неподдерживаемый xhttp mode",
			mutate: func(s *ManifestSnapshot) {
				s.Nodes[0].Public.Transport = TransportXHTTP
				s.Nodes[0].Public.XHTTP = &XHTTPConfig{Path: "/connect", Mode: "turbo"}
			},
			want: ErrManifestNodeInvalid,
		},
		{
			name: "xhttp недопустим в v1",
			mutate: func(s *ManifestSnapshot) {
				s.SchemaVersion = ManifestSchemaVersionV1
				s.Nodes[0].Public.Transport = TransportXHTTP
				s.Nodes[0].Public.XHTTP = &XHTTPConfig{Path: "/connect", Mode: "auto"}
			},
			want: ErrManifestNodeInvalid,
		},
		{
			name:   "неподдерживаемый flow",
			mutate: func(s *ManifestSnapshot) { s.Nodes[0].Public.Flow = "none" },
			want:   ErrManifestNodeInvalid,
		},

		// --- лимиты размера ---
		{
			name: "слишком много нод",
			mutate: func(s *ManifestSnapshot) {
				s.Nodes = make([]ManifestNode, MaxManifestNodes+1)
			},
			want: ErrManifestTooLarge,
		},
		{
			name: "слишком много нод во fleet",
			mutate: func(s *ManifestSnapshot) {
				s.Fleets[0].NodeIDs = make([]NodeID, MaxNodesPerFleet+1)
			},
			want: ErrManifestTooLarge,
		},
		{
			name: "слишком много fleets",
			mutate: func(s *ManifestSnapshot) {
				s.Fleets = make([]ManifestFleet, MaxManifestFleets+1)
			},
			want: ErrManifestTooLarge,
		},
		{
			name: "слишком много связей суммарно",
			mutate: func(s *ManifestSnapshot) {
				fleets := make([]ManifestFleet, 0, MaxManifestFleets)
				for i := range MaxManifestFleets {
					fleets = append(fleets, ManifestFleet{
						FleetID: int64(i + 1),
						Bridges: make([]ManifestBridge, MaxManifestRelations/MaxManifestFleets+1),
					})
				}
				s.Fleets = fleets
			},
			want: ErrManifestTooLarge,
		},
		{
			name:   "неположительный vpn_fleet_id",
			mutate: func(s *ManifestSnapshot) { s.Fleets[0].FleetID = 0 },
			want:   ErrManifestDuplicate,
		},
		{
			name:   "endpoint без хоста",
			mutate: func(s *ManifestSnapshot) { s.Nodes[0].Agent.Endpoint = ":9443" },
			want:   ErrManifestNodeInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := validSnapshot()
			tt.mutate(&snapshot)

			err := ValidateManifest(snapshot)
			if !errors.Is(err, tt.want) {
				t.Fatalf("ошибка %v, ожидалась %v", err, tt.want)
			}

			// Деталь обязана быть: голый сентинел не даёт CI понять, что чинить.
			var validation *ManifestValidationError
			if !errors.As(err, &validation) || validation.Detail == "" {
				t.Fatalf("ошибка %v не несёт детали для вызывающего", err)
			}
		})
	}
}

// TestManifestValidationErrorMessage — сообщение уходит наружу как есть, потому
// что manifest-writer это infrastructure CI/CD, поэтому его форма фиксируется.
func TestManifestValidationErrorMessage(t *testing.T) {
	withDetail := &ManifestValidationError{Rule: ErrManifestUnknownNode, Detail: "fleet 10 ссылается на ноду FR-9"}
	if got := withDetail.Error(); got != ErrManifestUnknownNode.Error()+": fleet 10 ссылается на ноду FR-9" {
		t.Errorf("сообщение %q", got)
	}

	bare := &ManifestValidationError{Rule: ErrManifestUnknownNode}
	if got := bare.Error(); got != ErrManifestUnknownNode.Error() {
		t.Errorf("сообщение без детали %q, ожидалось %q", got, ErrManifestUnknownNode)
	}
}

// TestValidateManifestAllowsEmptyFleet — fleet может временно не содержать
// ни нод, ни связей, но остаётся в каждом снапшоте.
func TestValidateManifestAllowsEmptyFleet(t *testing.T) {
	snapshot := validSnapshot()
	snapshot.Fleets = append(snapshot.Fleets, ManifestFleet{FleetID: 11})

	if err := ValidateManifest(snapshot); err != nil {
		t.Fatalf("пустой fleet отвергнут: %v", err)
	}
}

// TestValidateManifestAllowsEmptyShortID — обязательные поля перечислены
// явно, и short_id среди них нет: пустой sid — легальная конфигурация REALITY.
func TestValidateManifestAllowsEmptyShortID(t *testing.T) {
	snapshot := validSnapshot()
	snapshot.Nodes[0].Public.ShortID = ""

	if err := ValidateManifest(snapshot); err != nil {
		t.Fatalf("пустой short_id отвергнут: %v", err)
	}
}

// TestValidateManifestAllowsReversePair — пара направленная: NL->DE и DE->NL это
// две разные связи, а не дубликат.
func TestValidateManifestAllowsReversePair(t *testing.T) {
	snapshot := validSnapshot()
	snapshot.Fleets[0].Bridges = append(snapshot.Fleets[0].Bridges, ManifestBridge{
		RoutingKey:  "de-1.to-nl-1",
		EntryNodeID: "DE-1",
		ExitNodeID:  "NL-1",
		EgressTag:   "nl-exit",
	})

	if err := ValidateManifest(snapshot); err != nil {
		t.Fatalf("обратная связь отвергнута: %v", err)
	}
}
