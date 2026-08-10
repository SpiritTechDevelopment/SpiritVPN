package grpcsvc

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	manifestv1 "github.com/RomanRyabinkin/SpiritVPN/internal/gen/spiritvpn/manifest/v1"
)

// stubManifest — фейковый use case приёма манифеста.
type stubManifest struct {
	calls  int
	cmd    app.ApplyManifestCommand
	result app.ApplyManifestResult
	err    error
}

func (s *stubManifest) Execute(
	_ context.Context,
	cmd app.ApplyManifestCommand,
) (app.ApplyManifestResult, error) {
	s.calls++
	s.cmd = cmd
	return s.result, s.err
}

// manifestRequest — пример §6 на проводе.
func manifestRequest() *manifestv1.ApplyFleetManifestRequest {
	return &manifestv1.ApplyFleetManifestRequest{
		SchemaVersion: domain.ManifestSchemaVersion,
		Revision:      42,
		Nodes: []*manifestv1.ManifestNode{{
			NodeId: "NL-1",
			Agent: &manifestv1.NodeAgentConfig{
				Endpoint:            "10.0.0.11:9443",
				TlsServerName:       "nl-1.agent.internal",
				CertificateIdentity: "spiffe://spiritvpn/node/NL-1",
			},
			Public: &manifestv1.NodePublicConfig{
				Address:          "nl.example.com",
				Port:             443,
				RealityPublicKey: "pub-key",
				ServerName:       "www.example.org",
				ShortId:          "ab12",
				Fingerprint:      "chrome",
				Flow:             domain.FlowXTLSRprxVision,
				Transport:        domain.TransportTCP,
			},
			DisplayName: "Netherlands",
		}},
		Fleets: []*manifestv1.ManifestFleet{{
			VpnFleetId: 10,
			NodeIds:    []string{"NL-1"},
		}},
	}
}

// TestApplyFleetManifestMapsResult — §6: успех отдаёт APPLIED, идемпотентный
// повтор — IDEMPOTENT, и оба эхом возвращают revision.
func TestApplyFleetManifestMapsResult(t *testing.T) {
	tests := []struct {
		name   string
		result app.ApplyManifestResult
		want   manifestv1.ManifestApplyResult
	}{
		{
			name:   "новый снапшот",
			result: app.ApplyManifestResult{Revision: 42},
			want:   manifestv1.ManifestApplyResult_MANIFEST_APPLY_RESULT_APPLIED,
		},
		{
			name:   "идемпотентный повтор",
			result: app.ApplyManifestResult{Revision: 42, Idempotent: true},
			want:   manifestv1.ManifestApplyResult_MANIFEST_APPLY_RESULT_IDEMPOTENT,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewManifestServer(&stubManifest{result: tc.result})

			resp, err := srv.ApplyFleetManifest(context.Background(), manifestRequest())
			if err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}
			if resp.GetResult() != tc.want {
				t.Errorf("result %v, ожидался %v", resp.GetResult(), tc.want)
			}
			if resp.GetAppliedRevision() != 42 {
				t.Errorf("applied_revision %d, ожидалась 42", resp.GetAppliedRevision())
			}
		})
	}
}

// TestApplyFleetManifestConvertsRequest — раскладка §6 на проводе и доменная
// раскладка отличаются: display_name лежит рядом с node_id, а хранится вместе с
// публичными параметрами (решения 16, 19). Перекладка происходит на транспорте.
func TestApplyFleetManifestConvertsRequest(t *testing.T) {
	stub := &stubManifest{}
	srv := NewManifestServer(stub)

	req := manifestRequest()
	req.AllowDestructive = true

	if _, err := srv.ApplyFleetManifest(context.Background(), req); err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("use case вызван %d раз, ожидался 1", stub.calls)
	}

	snapshot := stub.cmd.Snapshot
	if snapshot.Revision != 42 || snapshot.SchemaVersion != domain.ManifestSchemaVersion {
		t.Errorf("шапка снапшота %+v", snapshot)
	}
	if !stub.cmd.AllowDestructive {
		t.Error("allow_destructive потерян")
	}

	node := snapshot.Nodes[0]
	if node.Public.DisplayName != "Netherlands" {
		t.Errorf("display_name %q не переложен в публичные параметры", node.Public.DisplayName)
	}
	if node.Public.Port != 443 || node.Agent.Endpoint != "10.0.0.11:9443" {
		t.Errorf("параметры ноды %+v", node)
	}

	// Снапшот обязан быть валидным по §6: транспорт ничего не теряет по дороге.
	if err := domain.ValidateManifest(snapshot); err != nil {
		t.Fatalf("собранный снапшот не проходит валидацию: %v", err)
	}
}

// TestApplyFleetManifestRejectsRevisionOverflow — колонка revision объявлена как
// bigint, и значение выше 2^63-1 обязано быть отвергнуто на границе, а не
// превратиться в отрицательное в адаптере (решение 11).
func TestApplyFleetManifestRejectsRevisionOverflow(t *testing.T) {
	stub := &stubManifest{}
	srv := NewManifestServer(stub)

	req := manifestRequest()
	req.Revision = math.MaxInt64 + 1

	_, err := srv.ApplyFleetManifest(context.Background(), req)
	st := requireCode(t, err, codes.InvalidArgument)

	if stub.calls != 0 {
		t.Errorf("use case вызван %d раз при переполнении revision", stub.calls)
	}
	if !strings.Contains(st.Message(), "2^63") {
		t.Errorf("сообщение %q не объясняет причину", st.Message())
	}
}

// TestApplyFleetManifestSurfacesValidationDetail — деталь нарушения §6 уходит
// вызывающему: он infrastructure CI/CD (§14), и голый INVALID_ARGUMENT заставил
// бы его разбирать снапшот вручную.
func TestApplyFleetManifestSurfacesValidationDetail(t *testing.T) {
	srv := NewManifestServer(&stubManifest{
		err: &domain.ManifestValidationError{
			Rule:   domain.ErrManifestUnknownNode,
			Detail: "связь 10/nl-1.to-de-1 ссылается на ноду DE-9",
		},
	})

	_, err := srv.ApplyFleetManifest(context.Background(), manifestRequest())
	st := requireCode(t, err, codes.InvalidArgument)

	if !strings.Contains(st.Message(), "DE-9") {
		t.Fatalf("сообщение %q не содержит деталь", st.Message())
	}
}

// TestApplyFleetManifestSplitsCodes — форма снапшота даёт INVALID_ARGUMENT,
// конфликт с принятым состоянием — FAILED_PRECONDITION.
func TestApplyFleetManifestSplitsCodes(t *testing.T) {
	tests := []struct {
		rule error
		want codes.Code
	}{
		{domain.ErrManifestSchemaVersion, codes.InvalidArgument},
		{domain.ErrManifestTooLarge, codes.InvalidArgument},
		{domain.ErrManifestDuplicate, codes.InvalidArgument},
		{domain.ErrManifestNodeInvalid, codes.InvalidArgument},
		{domain.ErrManifestBridgeInvalid, codes.InvalidArgument},

		{domain.ErrManifestRevisionRegression, codes.FailedPrecondition},
		{domain.ErrManifestDigestConflict, codes.FailedPrecondition},
		{domain.ErrManifestFleetMissing, codes.FailedPrecondition},
		{domain.ErrManifestDestructive, codes.FailedPrecondition},
		{domain.ErrManifestBridgePairImmutable, codes.FailedPrecondition},
	}

	for _, tc := range tests {
		t.Run(tc.rule.Error(), func(t *testing.T) {
			srv := NewManifestServer(&stubManifest{
				err: &domain.ManifestValidationError{Rule: tc.rule, Detail: "деталь"},
			})

			_, err := srv.ApplyFleetManifest(context.Background(), manifestRequest())
			requireCode(t, err, tc.want)
		})
	}
}

// TestApplyFleetManifestHidesInfrastructureErrors — обычная ошибка адаптера
// обезличивается, как и на командном пути: деталь разрешена только для
// ManifestValidationError.
func TestApplyFleetManifestHidesInfrastructureErrors(t *testing.T) {
	srv := NewManifestServer(&stubManifest{
		err: errors.New(`pq: relation "vpn_nodes" does not exist (host=db.internal)`),
	})

	_, err := srv.ApplyFleetManifest(context.Background(), manifestRequest())
	st := requireCode(t, err, codes.Internal)

	if st.Message() != msgInternal {
		t.Fatalf("сообщение %q, ожидалось обезличенное %q", st.Message(), msgInternal)
	}
}

// TestApplyFleetManifestPassesAuditFields — §15: аудиту нужны идентичность
// вызывающего и request_id, а взять их может только транспорт.
func TestApplyFleetManifestPassesAuditFields(t *testing.T) {
	stub := &stubManifest{}
	srv := NewManifestServer(stub)

	ctx := contextWithRequestID(tlsContext(certDNS("infra-ci")), "req-777")
	if _, err := srv.ApplyFleetManifest(ctx, manifestRequest()); err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}

	if stub.cmd.Actor != "infra-ci" {
		t.Errorf("actor %q, ожидался infra-ci", stub.cmd.Actor)
	}
	if stub.cmd.RequestID != "req-777" {
		t.Errorf("request_id %q, ожидался req-777", stub.cmd.RequestID)
	}
}
