package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	"github.com/RomanRyabinkin/SpiritVPN/internal/nodeagent"
)

// Юнит-тесты диспетчера. Смысл слоя — порядок фаз: порядок фаз требует, чтобы во время
// RPC не было открытой транзакции, а результат писался отдельной транзакцией с
// повторной проверкой desired_version. Гейт по ноде и SKIP LOCKED живут в SQL и
// проверяются интеграционно.

// fakeDispatchRepo ведёт журнал шагов: без него «вызов вне транзакции» не отличить
// от «вызова внутри неё».
type fakeDispatchRepo struct {
	journal []string

	reaped    int64
	reapErr   error
	leased    []*app.LeasedOperation
	leaseErr  error
	leaseTTLs []time.Duration

	// fresh — что вернёт SetAccessApplyState: false означает ушедшую вперёд версию.
	fresh      bool
	applyState domain.ApplyState
	results    []app.OperationResult
}

func (r *fakeDispatchRepo) ReapExpiredLeases(context.Context, int32) (int64, error) {
	r.journal = append(r.journal, "reap")
	return r.reaped, r.reapErr
}

func (r *fakeDispatchRepo) LeaseNext(
	_ context.Context,
	_ string,
	leaseTTL time.Duration,
) (*app.LeasedOperation, error) {
	r.journal = append(r.journal, "lease")
	r.leaseTTLs = append(r.leaseTTLs, leaseTTL)

	if r.leaseErr != nil {
		return nil, r.leaseErr
	}
	if len(r.leased) == 0 {
		return nil, nil
	}

	operation := r.leased[0]
	r.leased = r.leased[1:]
	return operation, nil
}

func (r *fakeDispatchRepo) WithinResultTx(ctx context.Context, fn func(app.ResultTx) error) error {
	r.journal = append(r.journal, "tx-begin")
	if err := fn(r); err != nil {
		return err
	}
	r.journal = append(r.journal, "tx-commit")
	return nil
}

func (r *fakeDispatchRepo) Now(context.Context) (time.Time, error) {
	return time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC), nil
}

func (r *fakeDispatchRepo) SetAccessApplyState(
	_ context.Context,
	_ uuid.UUID,
	_ int64,
	state domain.ApplyState,
) (bool, error) {
	r.journal = append(r.journal, "apply-state")
	r.applyState = state
	return r.fresh, nil
}

func (r *fakeDispatchRepo) CompleteOperation(_ context.Context, result app.OperationResult) error {
	r.journal = append(r.journal, "complete")
	r.results = append(r.results, result)
	return nil
}

// fakeAgentDispatcher скриптует исход доставки и запоминает, что уехало.
type fakeAgentDispatcher struct {
	outcome nodeagent.Outcome

	journal  *[]string
	present  []nodeagent.User
	absent   []string
	endpoint nodeagent.Endpoint
}

func (a *fakeAgentDispatcher) EnsureUserPresent(
	_ context.Context,
	endpoint nodeagent.Endpoint,
	_ string,
	user nodeagent.User,
) nodeagent.Outcome {
	*a.journal = append(*a.journal, "rpc")
	a.endpoint = endpoint
	a.present = append(a.present, user)
	return a.outcome
}

func (a *fakeAgentDispatcher) EnsureUserAbsent(
	_ context.Context,
	endpoint nodeagent.Endpoint,
	_ string,
	accountingID string,
) nodeagent.Outcome {
	*a.journal = append(*a.journal, "rpc")
	a.endpoint = endpoint
	a.absent = append(a.absent, accountingID)
	return a.outcome
}

// fixedJitter делает backoff детерминированным.
type fixedJitter float64

func (j fixedJitter) Unit() float64 { return float64(j) }

// testSealer — настоящий Cipher, а не stubSealer соседних тестов: диспетчер
// единственный, кто расшифровывает credential, и подмена Open скрыла бы ровно то,
// что здесь проверяется — что на ноду уезжает тот самый client_uuid.
func testSealer(t *testing.T) *crypto.Cipher {
	t.Helper()

	key, err := crypto.NewKey("test-key", make([]byte, crypto.KeySize))
	if err != nil {
		t.Fatalf("crypto.NewKey: %v", err)
	}
	cipher, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatalf("crypto.NewCipher: %v", err)
	}
	return cipher
}

func sealTestUUID(t *testing.T) crypto.SealedCredential {
	t.Helper()

	credential, err := testSealer(t).Seal(testClientUUID)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return credential
}

const dispatchTestTTL = 30 * time.Second

func newDispatchHarness(
	t *testing.T,
	operation *app.LeasedOperation,
	outcome nodeagent.Outcome,
) (*app.DispatchOperations, *fakeDispatchRepo, *fakeAgentDispatcher) {
	t.Helper()

	repo := &fakeDispatchRepo{fresh: true}
	if operation != nil {
		repo.leased = []*app.LeasedOperation{operation}
	}
	agent := &fakeAgentDispatcher{outcome: outcome, journal: &repo.journal}

	return app.NewDispatchOperations(
		repo, agent, testSealer(t), fixedJitter(0), "worker-1", dispatchTestTTL,
	), repo, agent
}

// presentOperation — типовая операция доставки PRESENT.
func presentOperation(t *testing.T) *app.LeasedOperation {
	t.Helper()

	return &app.LeasedOperation{
		OperationID:          uuid.New(),
		AccessID:             uuid.New(),
		DesiredVersion:       7,
		AttemptCount:         1,
		DesiredState:         domain.DesiredStatePresent,
		AccessDesiredVersion: 7,
		Endpoint: nodeagent.Endpoint{
			NodeID:              "node-a",
			Address:             "10.0.0.1:9443",
			TLSServerName:       "node-a.agents.internal",
			CertificateIdentity: "spiffe://spiritvpn/node/node-a",
		},
		AccountingID: "acc-1",
		EgressKey:    "exit-de",
		Flow:         "xtls-rprx-vision",
		Credential:   sealTestUUID(t),
	}
}

func applied() nodeagent.Outcome {
	return nodeagent.Outcome{Result: domain.AttemptSucceeded, Code: nodeagent.CodeApplied}
}

// TestDispatchCallsAgentOutsideTransaction — ни одна транзакция не остаётся
// открытой во время обращения к node-agent.
func TestDispatchCallsAgentOutsideTransaction(t *testing.T) {
	uc, repo, _ := newDispatchHarness(t, presentOperation(t), applied())

	progressed, err := uc.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if !progressed {
		t.Fatal("доставка не засчитана прогрессом")
	}

	want := []string{"reap", "lease", "rpc", "tx-begin", "apply-state", "complete", "tx-commit"}
	if !equalSteps(repo.journal, want) {
		t.Fatalf("порядок шагов %v, ожидался %v", repo.journal, want)
	}
}

// TestDispatchDeliversPayloadFromAccessRow — payload собирается из строки
// access, а не хранится в operation.
func TestDispatchDeliversPayloadFromAccessRow(t *testing.T) {
	operation := presentOperation(t)
	uc, repo, agent := newDispatchHarness(t, operation, applied())

	if _, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	if len(agent.present) != 1 {
		t.Fatalf("вызовов EnsureUserPresent %d, ожидался 1", len(agent.present))
	}
	user := agent.present[0]

	if user.AccountingID != operation.AccountingID {
		t.Errorf("accounting_id %q, ожидался %q", user.AccountingID, operation.AccountingID)
	}
	if user.EgressKey != operation.EgressKey {
		t.Errorf("egress_key %q, ожидался %q", user.EgressKey, operation.EgressKey)
	}
	if user.Flow != operation.Flow {
		t.Errorf("flow %q, ожидался %q", user.Flow, operation.Flow)
	}
	if user.ClientUUID.Reveal() != testClientUUID.Reveal() {
		t.Error("на ноду уехал не тот client_uuid")
	}
	if agent.endpoint != operation.Endpoint {
		t.Errorf("endpoint %+v, ожидался %+v", agent.endpoint, operation.Endpoint)
	}

	if repo.applyState != domain.ApplyStateApplied {
		t.Errorf("apply_state %s, ожидался APPLIED", repo.applyState)
	}
	if repo.results[0].Status != domain.OperationStatusSucceeded {
		t.Errorf("статус операции %s, ожидался SUCCEEDED", repo.results[0].Status)
	}
	if repo.leaseTTLs[0] != dispatchTestTTL {
		t.Errorf("lease TTL %s, ожидался %s", repo.leaseTTLs[0], dispatchTestTTL)
	}
}

// TestDispatchAbsentSendsNoCredential — удаление матчится по accounting_id,
// и расшифровывать credential ради него незачем.
func TestDispatchAbsentSendsNoCredential(t *testing.T) {
	operation := presentOperation(t)
	operation.DesiredState = domain.DesiredStateAbsent
	operation.Credential = crypto.SealedCredential{}

	uc, _, agent := newDispatchHarness(t, operation, applied())

	if _, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	if len(agent.absent) != 1 || agent.absent[0] != operation.AccountingID {
		t.Fatalf("вызовы EnsureUserAbsent %v", agent.absent)
	}
	if len(agent.present) != 0 {
		t.Error("для ABSENT вызван EnsureUserPresent")
	}
}

// TestDispatchRetryableSchedulesNextAttempt — временный отказ повторяется с
// backoff от времени БАЗЫ, а не от часов процесса.
func TestDispatchRetryableSchedulesNextAttempt(t *testing.T) {
	uc, repo, _ := newDispatchHarness(t, presentOperation(t), nodeagent.Outcome{
		Result: domain.AttemptRetryable,
		Code:   nodeagent.CodeUnavailable,
	})

	if _, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	result := repo.results[0]
	if result.Status != domain.OperationStatusRetryWait {
		t.Fatalf("статус %s, ожидался RETRY_WAIT", result.Status)
	}
	if result.NextAttemptAt == nil {
		t.Fatal("нет next_attempt_at: операцию никто не подхватит")
	}
	// attempt_count = 1 (увеличен при взятии lease), jitter = 0 —
	// нижняя половина от 2 секунд.
	txNow, _ := repo.Now(context.Background())
	if want := txNow.Add(time.Second); !result.NextAttemptAt.Equal(want) {
		t.Errorf("next_attempt_at %v, ожидалось %v", result.NextAttemptAt, want)
	}
	if repo.applyState != domain.ApplyStateRetrying {
		t.Errorf("apply_state %s, ожидался RETRYING", repo.applyState)
	}
}

// TestDispatchPermanentIsTerminal — permanent не хот-лупится.
func TestDispatchPermanentIsTerminal(t *testing.T) {
	uc, repo, _ := newDispatchHarness(t, presentOperation(t), nodeagent.Outcome{
		Result: domain.AttemptPermanent,
		Code:   nodeagent.CodeInvalidArgument,
	})

	if _, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	result := repo.results[0]
	if result.Status != domain.OperationStatusFailedPermanent {
		t.Errorf("статус %s, ожидался FAILED_PERMANENT", result.Status)
	}
	if result.NextAttemptAt != nil {
		t.Errorf("permanent запланирован на %v", result.NextAttemptAt)
	}
	if repo.applyState != domain.ApplyStateFailed {
		t.Errorf("apply_state %s, ожидался FAILED", repo.applyState)
	}
}

// TestDispatchSkipsRPCWhenDesiredVersionMoved — перед RPC проверяется
// актуальная desired_version, и устаревшую операцию агенту не отправляют.
func TestDispatchSkipsRPCWhenDesiredVersionMoved(t *testing.T) {
	operation := presentOperation(t)
	operation.AccessDesiredVersion = operation.DesiredVersion + 1

	uc, repo, agent := newDispatchHarness(t, operation, applied())
	// Строка access уже другой версии — guarded UPDATE её не найдёт.
	repo.fresh = false

	progressed, err := uc.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if !progressed {
		t.Fatal("снятие устаревшей операции с очереди — это прогресс")
	}

	if len(agent.present)+len(agent.absent) != 0 {
		t.Error("агенту уехало заведомо устаревшее состояние")
	}
	if repo.results[0].Status != domain.OperationStatusSuperseded {
		t.Errorf("статус %s, ожидался SUPERSEDED", repo.results[0].Status)
	}
	if repo.results[0].ErrorCode != app.CodeSuperseded {
		t.Errorf("код %q, ожидался %q", repo.results[0].ErrorCode, app.CodeSuperseded)
	}
}

// TestDispatchStaleResultDoesNotOverwriteApplyState — результат устаревшей
// операции не меняет apply_state актуальной desired version, а сама операция
// больше не повторяется.
func TestDispatchStaleResultDoesNotOverwriteApplyState(t *testing.T) {
	uc, repo, _ := newDispatchHarness(t, presentOperation(t), nodeagent.Outcome{
		Result: domain.AttemptRetryable,
		Code:   nodeagent.CodeUnavailable,
	})
	// desired_version ушла вперёд, пока шёл RPC.
	repo.fresh = false

	if _, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	result := repo.results[0]
	if result.Status != domain.OperationStatusSuperseded {
		t.Errorf("статус %s, ожидался SUPERSEDED", result.Status)
	}
	if result.NextAttemptAt != nil {
		t.Error("устаревшая операция запланирована на повтор")
	}
	if !result.Completed {
		t.Error("SUPERSEDED терминален")
	}
}

// TestDispatchSucceededStaleStaysSucceeded — терминальный исход правдив даже для
// устаревшей версии: agent_operations — ещё и журнал исполнения.
func TestDispatchSucceededStaleStaysSucceeded(t *testing.T) {
	uc, repo, _ := newDispatchHarness(t, presentOperation(t), applied())
	repo.fresh = false

	if _, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	if repo.results[0].Status != domain.OperationStatusSucceeded {
		t.Errorf("статус %s, ожидался SUCCEEDED", repo.results[0].Status)
	}
}

// TestDispatchIdleReportsNoProgress — пустая очередь означает, что циклу пора
// подождать.
func TestDispatchIdleReportsNoProgress(t *testing.T) {
	uc, repo, _ := newDispatchHarness(t, nil, applied())

	progressed, err := uc.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if progressed {
		t.Error("пустая очередь засчитана прогрессом")
	}
	if len(repo.results) != 0 {
		t.Error("на пустой очереди что-то записано")
	}
}

// TestDispatchReapedLeaseIsProgress — собранный протухший lease вернул операцию в
// очередь, и следующий шаг её заберёт: ждать незачем.
func TestDispatchReapedLeaseIsProgress(t *testing.T) {
	uc, repo, _ := newDispatchHarness(t, nil, applied())
	repo.reaped = 3

	progressed, err := uc.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if !progressed {
		t.Error("сбор протухших lease не засчитан прогрессом")
	}
}

// TestDispatchCancelledContextWritesNothing — отмена во время RPC не
// пишет результат. Операция остаётся IN_FLIGHT и достаётся сборщику lease.
func TestDispatchCancelledContextWritesNothing(t *testing.T) {
	uc, repo, agent := newDispatchHarness(t, presentOperation(t), applied())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	progressed, err := uc.ProcessNext(ctx)
	if err != nil {
		t.Fatalf("отмена — штатная остановка, а не отказ: %v", err)
	}
	if progressed {
		t.Error("несостоявшаяся доставка засчитана прогрессом")
	}
	if len(agent.present)+len(agent.absent) != 0 {
		t.Error("RPC пошёл на отменённом контексте")
	}
	if len(repo.results) != 0 {
		t.Error("результат записан отменённой транзакцией")
	}
}

// TestDispatchUnreadableCredentialRetriesWithAlert — нерасшифровываемый credential
// не должен стать вечным FAILED: сменой desired state его не починить, потому что
// провалится и следующее поколение.
func TestDispatchUnreadableCredentialRetriesWithAlert(t *testing.T) {
	operation := presentOperation(t)
	operation.Credential = crypto.SealedCredential{Blob: []byte("не шифротекст"), KeyID: "test-key"}

	uc, repo, agent := newDispatchHarness(t, operation, applied())

	if _, err := uc.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	if len(agent.present) != 0 {
		t.Error("вызов ушёл агенту без расшифрованного credential")
	}
	result := repo.results[0]
	if result.Status != domain.OperationStatusRetryWait {
		t.Errorf("статус %s, ожидался RETRY_WAIT", result.Status)
	}
	if result.ErrorCode != app.CodeCredentialUnreadable {
		t.Errorf("код %q, ожидался %q", result.ErrorCode, app.CodeCredentialUnreadable)
	}
}

// TestDispatchPropagatesRepositoryErrors — отказ БАЗЫ обязан дойти до цикла: это
// не исход операции, а сбой шага.
func TestDispatchPropagatesRepositoryErrors(t *testing.T) {
	broken := errors.New("база недоступна")

	t.Run("сбор lease", func(t *testing.T) {
		uc, repo, _ := newDispatchHarness(t, nil, applied())
		repo.reapErr = broken

		if _, err := uc.ProcessNext(context.Background()); !errors.Is(err, broken) {
			t.Fatalf("ошибка %v, ожидалась %v", err, broken)
		}
	})

	t.Run("взятие операции", func(t *testing.T) {
		uc, repo, _ := newDispatchHarness(t, nil, applied())
		repo.leaseErr = broken

		if _, err := uc.ProcessNext(context.Background()); !errors.Is(err, broken) {
			t.Fatalf("ошибка %v, ожидалась %v", err, broken)
		}
	})
}

func equalSteps(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
