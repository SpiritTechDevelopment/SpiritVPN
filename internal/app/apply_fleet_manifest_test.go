package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
)

// Тесты приёма манифеста проверяют то, чего не видно ни в домене, ни в SQL:
// ЧТО именно пишется при каждом исходе. Идемпотентный повтор не пишет ничего,
// обычный приём пишет проекцию без аудита, destructive — проекцию и аудит.

// fakeManifestTx ведёт журнал вызовов: порядок и состав важнее аргументов.
type fakeManifestTx struct {
	journal    []string
	projection domain.ManifestProjection

	plan   domain.ManifestPlan
	audits []app.AuditEvent

	loadErr  error
	writeErr error
}

func (tx *fakeManifestTx) LoadProjection(context.Context) (domain.ManifestProjection, error) {
	tx.journal = append(tx.journal, "LoadProjection")
	return tx.projection, tx.loadErr
}

func (tx *fakeManifestTx) WritePlan(_ context.Context, plan domain.ManifestPlan) error {
	tx.journal = append(tx.journal, "WritePlan")
	tx.plan = plan
	return tx.writeErr
}

func (tx *fakeManifestTx) AppendAudit(_ context.Context, event app.AuditEvent) error {
	tx.journal = append(tx.journal, "AppendAudit")
	tx.audits = append(tx.audits, event)
	return nil
}

type fakeManifestRepo struct {
	tx        *fakeManifestTx
	committed bool
}

func (r *fakeManifestRepo) WithinManifestTx(ctx context.Context, fn func(app.ManifestTx) error) error {
	if err := fn(r.tx); err != nil {
		return err
	}
	r.committed = true
	return nil
}

// manifestNode повторяет фикстуру доменных тестов: идентификаторы ASCII, все
// обязательные поля §6 заполнены.
func manifestNode(id string) domain.ManifestNode {
	return domain.ManifestNode{
		NodeID: domain.NodeID(id),
		Agent: domain.NodeAgent{
			Endpoint:            "10.0.0.11:9443",
			TLSServerName:       id + ".agent.internal",
			CertificateIdentity: "spiffe://spiritvpn/node/" + id,
		},
		Public: domain.NodePublic{
			Address:          id + ".example.com",
			Port:             443,
			RealityPublicKey: "pub-key",
			ServerName:       "www.example.org",
			ShortID:          "ab12",
			Fingerprint:      "chrome",
			Transport:        domain.TransportTCP,
			Flow:             domain.FlowXTLSRprxVision,
			DisplayName:      id,
		},
	}
}

func manifestSnapshot() domain.ManifestSnapshot {
	return domain.ManifestSnapshot{
		SchemaVersion: domain.ManifestSchemaVersion,
		Revision:      42,
		Nodes:         []domain.ManifestNode{manifestNode("NL-1"), manifestNode("DE-1")},
		Fleets: []domain.ManifestFleet{{
			FleetID: 10,
			NodeIDs: []domain.NodeID{"NL-1", "DE-1"},
		}},
	}
}

func newManifestHarness(tx *fakeManifestTx) (*app.ApplyFleetManifest, *fakeManifestRepo) {
	repo := &fakeManifestRepo{tx: tx}
	return app.NewApplyFleetManifest(repo), repo
}

func manifestCommand() app.ApplyManifestCommand {
	return app.ApplyManifestCommand{
		Snapshot:  manifestSnapshot(),
		Actor:     "infra-ci",
		RequestID: "req-1",
	}
}

// TestApplyManifestWritesProjection — обычный приём: проекция записана, аудита
// нет (§15 требует его только для destructive).
func TestApplyManifestWritesProjection(t *testing.T) {
	tx := &fakeManifestTx{}
	uc, repo := newManifestHarness(tx)

	result, err := uc.Execute(context.Background(), manifestCommand())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if result.Idempotent || result.Revision != 42 {
		t.Fatalf("результат %+v, ожидался обычный приём revision 42", result)
	}
	if !repo.committed {
		t.Fatal("транзакция не закоммичена")
	}

	want := []string{"LoadProjection", "WritePlan"}
	if len(tx.journal) != len(want) {
		t.Fatalf("журнал %v, ожидался %v", tx.journal, want)
	}
	for i, step := range want {
		if tx.journal[i] != step {
			t.Fatalf("журнал %v, ожидался %v", tx.journal, want)
		}
	}
	if len(tx.plan.Payload) == 0 || tx.plan.Digest == "" {
		t.Error("в проекцию не уехал канонический payload с digest")
	}
}

// TestApplyManifestIdempotentWritesNothing — решение 21: повтор не пишет вовсе
// ничего, включая джобу материализации.
func TestApplyManifestIdempotentWritesNothing(t *testing.T) {
	snapshot := manifestSnapshot()
	_, digest := domain.CanonicalizeManifest(snapshot)

	tx := &fakeManifestTx{projection: domain.ManifestProjection{
		LastRevision: snapshot.Revision,
		LastDigest:   digest,
		Fleets:       []int64{10},
	}}
	uc, repo := newManifestHarness(tx)

	result, err := uc.Execute(context.Background(), manifestCommand())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !result.Idempotent {
		t.Fatal("повтор не признан идемпотентным")
	}
	if result.Revision != snapshot.Revision {
		t.Errorf("revision %d, ожидалась %d", result.Revision, snapshot.Revision)
	}
	if len(tx.journal) != 1 || tx.journal[0] != "LoadProjection" {
		t.Fatalf("журнал %v, ожидалось только чтение", tx.journal)
	}
	// Транзакция всё равно коммитится: писать нечего, но и откатывать нечего.
	if !repo.committed {
		t.Error("идемпотентный повтор откатил транзакцию")
	}
}

// TestApplyManifestAuditsDestructive — §15: audit обязателен для destructive
// manifest, и пишется он в той же транзакции, что и проекция.
func TestApplyManifestAuditsDestructive(t *testing.T) {
	accepted := manifestSnapshot()

	tx := &fakeManifestTx{projection: domain.ManifestProjection{
		LastRevision: accepted.Revision,
		LastDigest:   "другой",
		Fleets:       []int64{10},
		CurrentNodes: []domain.NodeID{"NL-1", "DE-1"},
		CurrentMemberships: []domain.FleetNodeKey{
			{FleetID: 10, NodeID: "NL-1"},
			{FleetID: 10, NodeID: "DE-1"},
		},
	}}
	uc, _ := newManifestHarness(tx)

	cmd := manifestCommand()
	cmd.Snapshot.Revision = accepted.Revision + 1
	cmd.Snapshot.Nodes = []domain.ManifestNode{manifestNode("NL-1")}
	cmd.Snapshot.Fleets[0].NodeIDs = []domain.NodeID{"NL-1"}
	cmd.AllowDestructive = true

	if _, err := uc.Execute(context.Background(), cmd); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(tx.journal) != 3 || tx.journal[2] != "AppendAudit" {
		t.Fatalf("журнал %v, ожидалась запись аудита после проекции", tx.journal)
	}

	audit := tx.audits[0]
	if audit.ActorID != "infra-ci" || audit.RequestID != "req-1" {
		t.Errorf("аудит не несёт идентичность и request_id: %+v", audit)
	}
	if audit.Metadata["destructive"] != true {
		t.Errorf("метаданные аудита %+v", audit.Metadata)
	}
}

// TestApplyManifestNoAuditWithoutRemovals — §15 требует аудит только для
// destructive; обычный приём журнал не засоряет.
func TestApplyManifestNoAuditWithoutRemovals(t *testing.T) {
	tx := &fakeManifestTx{}
	uc, _ := newManifestHarness(tx)

	if _, err := uc.Execute(context.Background(), manifestCommand()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(tx.audits) != 0 {
		t.Fatalf("аудит записан для не-destructive приёма: %+v", tx.audits)
	}
}

// TestApplyManifestValidatesBeforeTransaction — невалидный снапшот не должен ни
// брать advisory lock, ни читать проекцию целиком.
func TestApplyManifestValidatesBeforeTransaction(t *testing.T) {
	tx := &fakeManifestTx{}
	uc, repo := newManifestHarness(tx)

	cmd := manifestCommand()
	cmd.Snapshot.SchemaVersion = 2

	_, err := uc.Execute(context.Background(), cmd)
	if !errors.Is(err, domain.ErrManifestSchemaVersion) {
		t.Fatalf("ошибка %v, ожидалась ErrManifestSchemaVersion", err)
	}
	if len(tx.journal) != 0 || repo.committed {
		t.Fatalf("невалидный снапшот дошёл до транзакции: %v", tx.journal)
	}
}

// TestApplyManifestPropagatesPlanError — отказ по состоянию откатывает
// транзакцию и доезжает до транспорта нетронутым.
func TestApplyManifestPropagatesPlanError(t *testing.T) {
	tx := &fakeManifestTx{projection: domain.ManifestProjection{
		LastRevision: 100,
		Fleets:       []int64{10},
	}}
	uc, repo := newManifestHarness(tx)

	_, err := uc.Execute(context.Background(), manifestCommand())
	if !errors.Is(err, domain.ErrManifestRevisionRegression) {
		t.Fatalf("ошибка %v, ожидалась ErrManifestRevisionRegression", err)
	}
	if repo.committed {
		t.Fatal("транзакция закоммичена после отказа планировщика")
	}
	if len(tx.journal) != 1 {
		t.Fatalf("журнал %v, ожидалось только чтение", tx.journal)
	}
}

// TestApplyManifestPropagatesWriteError — отказ записи не превращается в
// успешный ответ.
func TestApplyManifestPropagatesWriteError(t *testing.T) {
	writeFailed := errors.New("нет связи с базой")
	tx := &fakeManifestTx{writeErr: writeFailed}
	uc, repo := newManifestHarness(tx)

	result, err := uc.Execute(context.Background(), manifestCommand())
	if !errors.Is(err, writeFailed) {
		t.Fatalf("ошибка %v, ожидался проброс %v", err, writeFailed)
	}
	if repo.committed {
		t.Fatal("транзакция закоммичена после отказа записи")
	}
	if result.Revision != 0 {
		t.Errorf("при ошибке возвращён результат %+v", result)
	}
}
