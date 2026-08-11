package app_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
)

// Тесты use case проверяют то, чего не видно ни в домене, ни в SQL: ПОРЯДОК шагов
// внутри транзакции. §5 и §11.1 задают его нормативно, и нарушения дают тихие
// баги — например, повтор старой команды для исчезнувшего fleet вернул бы
// NOT_FOUND вместо идемпотентного OK.
//
// Поэтому фейковая транзакция не просто отдаёт данные, а записывает журнал
// вызовов, и каждый тест утверждает про этот журнал.

// --- фейки ------------------------------------------------------------------

// fakeTx записывает вызовы и отдаёт заранее подготовленное состояние.
type fakeTx struct {
	calls []string

	now          time.Time
	entitlement  *domain.Entitlement
	fleetCurrent bool
	openPeriod   *domain.QuotaPeriod
	nodeUsage    []domain.NodeQuotaUsage
	topology     domain.FleetTopology
	accesses     []domain.Access

	written *app.MaterializedPlan
	audits  []app.AuditEvent
}

func (tx *fakeTx) record(call string) { tx.calls = append(tx.calls, call) }

func (tx *fakeTx) Now(context.Context) (time.Time, error) {
	tx.record("Now")
	return tx.now, nil
}

func (tx *fakeTx) LockEntitlement(context.Context, string) (*domain.Entitlement, error) {
	tx.record("LockEntitlement")
	return tx.entitlement, nil
}

func (tx *fakeTx) FleetIsCurrent(context.Context, int64) (bool, error) {
	tx.record("FleetIsCurrent")
	return tx.fleetCurrent, nil
}

func (tx *fakeTx) LockOpenQuotaPeriod(context.Context, string) (*domain.QuotaPeriod, error) {
	tx.record("LockOpenQuotaPeriod")
	return tx.openPeriod, nil
}

func (tx *fakeTx) LockNodeQuotaUsage(context.Context, uuid.UUID) ([]domain.NodeQuotaUsage, error) {
	tx.record("LockNodeQuotaUsage")
	return tx.nodeUsage, nil
}

func (tx *fakeTx) LoadTopology(context.Context, int64) (domain.FleetTopology, error) {
	tx.record("LoadTopology")
	return tx.topology, nil
}

func (tx *fakeTx) LoadAccesses(context.Context, string) ([]domain.Access, error) {
	tx.record("LoadAccesses")
	return tx.accesses, nil
}

func (tx *fakeTx) AppendAudit(_ context.Context, event app.AuditEvent) error {
	tx.record("AppendAudit")
	tx.audits = append(tx.audits, event)
	return nil
}

func (tx *fakeTx) WritePlan(_ context.Context, plan app.MaterializedPlan) error {
	tx.record("WritePlan")
	tx.written = &plan
	return nil
}

// fakeRepo отдаёт одну и ту же транзакцию и запоминает, коммитилась ли она.
type fakeRepo struct {
	tx        *fakeTx
	opened    int
	committed bool
}

func (r *fakeRepo) WithinTx(ctx context.Context, fn func(app.ApplyTx) error) error {
	r.opened++
	err := fn(r.tx)
	r.committed = err == nil
	return err
}

// countingIDs выдаёт предсказуемые идентификаторы и считает выдачи, чтобы тест мог
// утверждать, что лишних credentials не сгенерировано.
type countingIDs struct {
	accessIDs, periodIDs, operationIDs, accountingIDs, clientUUIDs int
}

func (g *countingIDs) NewAccessID() (uuid.UUID, error) {
	g.accessIDs++
	return uuid.New(), nil
}

func (g *countingIDs) NewQuotaPeriodID() (uuid.UUID, error) {
	g.periodIDs++
	return uuid.New(), nil
}

func (g *countingIDs) NewOperationID() (uuid.UUID, error) {
	g.operationIDs++
	return uuid.New(), nil
}

func (g *countingIDs) NewAccountingID() (string, error) {
	g.accountingIDs++
	return fmt.Sprintf("u.testaccountingid%05d", g.accountingIDs), nil
}

func (g *countingIDs) NewClientUUID() (crypto.ClientUUID, error) {
	g.clientUUIDs++
	return crypto.NewClientUUID(uuid.New()), nil
}

// stubSealer подменяет шифрование: сам Cipher покрыт тестами пакета crypto, а use
// case интересует только то, что Seal вызван на каждый новый access.
type stubSealer struct {
	sealed int
	err    error
}

func (s *stubSealer) Seal(crypto.ClientUUID) (crypto.SealedCredential, error) {
	if s.err != nil {
		return crypto.SealedCredential{}, s.err
	}
	s.sealed++
	return crypto.SealedCredential{KeyID: "test", Blob: make([]byte, crypto.SealedBlobSize)}, nil
}

func (s *stubSealer) Open(crypto.SealedCredential) (crypto.ClientUUID, error) {
	return crypto.ClientUUID{}, errors.New("не используется")
}

func (s *stubSealer) KeyID() string { return "test" }

// --- обвязка ----------------------------------------------------------------

var testNow = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

// Идентичность вызывающего и корреляция запроса; уезжают в audit_events (§15).
const (
	testActor     = "product-svc"
	testRequestID = "req-1"
)

func newHarness(tx *fakeTx) (*app.ApplyCustomerAccess, *fakeRepo, *countingIDs, *stubSealer) {
	repo := &fakeRepo{tx: tx}
	ids := &countingIDs{}
	sealer := &stubSealer{}
	return app.NewApplyCustomerAccess(repo, ids, sealer), repo, ids, sealer
}

// request оборачивает доменную команду в то, что принимает use case.
//
// validCommand остаётся доменной, потому что тесты правил §5 меняют её поля:
// actor и request_id к этим правилам отношения не имеют и добавляются здесь.
func request(cmd domain.ApplyCommand) app.ApplyCustomerCommand {
	return app.ApplyCustomerCommand{Command: cmd, Actor: testActor, RequestID: testRequestID}
}

func validCommand() domain.ApplyCommand {
	return domain.ApplyCommand{
		CustomerID:      "customer-1",
		FleetID:         7,
		UsageQuotaBytes: 1 << 30,
		ExpiresAt:       testNow.Add(30 * 24 * time.Hour),
		CommandNumber:   5,
	}
}

// --- тесты ------------------------------------------------------------------

// Невалидный запрос не должен ни открывать транзакцию, ни блокировать корневую
// строку, ни двигать last_command_number (§5, шаг 1).
func TestExecuteValidatesBeforeTouchingDatabase(t *testing.T) {
	commands := map[string]func(*domain.ApplyCommand){
		"пустой customer_id": func(c *domain.ApplyCommand) { c.CustomerID = "" },
		"нулевой fleet":      func(c *domain.ApplyCommand) { c.FleetID = 0 },
		"нулевая квота":      func(c *domain.ApplyCommand) { c.UsageQuotaBytes = 0 },
		"нулевой номер":      func(c *domain.ApplyCommand) { c.CommandNumber = 0 },
	}

	for name, mutate := range commands {
		t.Run(name, func(t *testing.T) {
			tx := &fakeTx{now: testNow, fleetCurrent: true}
			uc, repo, _, _ := newHarness(tx)

			cmd := validCommand()
			mutate(&cmd)

			if err := uc.Execute(context.Background(), request(cmd)); err == nil {
				t.Fatal("ожидалась ошибка валидации")
			}
			if repo.opened != 0 {
				t.Fatalf("транзакция открыта %d раз(а), ожидалось 0", repo.opened)
			}
		})
	}
}

// Устаревшая команда завершается идемпотентным OK без единого side effect, и
// проверка идёт ДО поиска fleet: иначе повтор старой команды для исчезнувшего
// fleet вернул бы NOT_FOUND вместо OK (§5, правило 2).
func TestExecuteStaleCommandStopsBeforeFleetLookup(t *testing.T) {
	tx := &fakeTx{
		now: testNow,
		entitlement: &domain.Entitlement{
			FleetID:           7,
			ExpiresAt:         testNow.Add(time.Hour),
			LastCommandNumber: 5,
		},
		fleetCurrent: false, // fleet уже исчез из manifest
	}
	uc, repo, ids, sealer := newHarness(tx)

	cmd := validCommand()
	cmd.CommandNumber = 5 // не больше сохранённого

	if err := uc.Execute(context.Background(), request(cmd)); err != nil {
		t.Fatalf("устаревшая команда вернула ошибку: %v", err)
	}

	want := []string{"Now", "LockEntitlement"}
	if !slices.Equal(tx.calls, want) {
		t.Fatalf("вызовы %v, ожидалось %v", tx.calls, want)
	}
	if tx.written != nil {
		t.Fatal("устаревшая команда не должна писать план")
	}
	if !repo.committed {
		t.Fatal("устаревшая команда должна завершаться commit, а не rollback")
	}
	if ids.accessIDs+ids.periodIDs+ids.operationIDs != 0 || sealer.sealed != 0 {
		t.Fatal("устаревшая команда не должна генерировать идентификаторы")
	}
}

// SELECT now() — первый оператор транзакции, и он предшествует блокировке корневой
// строки (решение 2, §11.1).
func TestExecuteReadsTransactionTimeFirst(t *testing.T) {
	tx := &fakeTx{
		now:          testNow,
		fleetCurrent: true,
		topology:     domain.FleetTopology{FleetID: 7, Nodes: []domain.NodeID{"node-a"}},
	}
	uc, _, _, _ := newHarness(tx)

	if err := uc.Execute(context.Background(), request(validCommand())); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(tx.calls) == 0 || tx.calls[0] != "Now" {
		t.Fatalf("первый вызов %v, ожидался Now", tx.calls)
	}
	if tx.calls[1] != "LockEntitlement" {
		t.Fatalf("второй вызов %q, ожидался LockEntitlement", tx.calls[1])
	}
}

// Неизвестный fleet возвращает NOT_FOUND и не пишет плана (§5, правило 6).
func TestExecuteUnknownFleet(t *testing.T) {
	tx := &fakeTx{now: testNow, fleetCurrent: false}
	uc, repo, _, _ := newHarness(tx)

	err := uc.Execute(context.Background(), request(validCommand()))
	if !errors.Is(err, domain.ErrFleetNotFound) {
		t.Fatalf("Execute = %v, ожидалась ErrFleetNotFound", err)
	}
	if tx.written != nil {
		t.Fatal("отклонённая команда не должна писать план")
	}
	if repo.committed {
		t.Fatal("отклонённая команда обязана откатывать транзакцию: номер не двигается (§5, правило 3)")
	}
}

// Первый Apply создаёт корневую строку, открывает период, материализует access под
// топологию и выпускает по операции на каждый PRESENT (§5).
func TestExecuteCreatesCustomer(t *testing.T) {
	tx := &fakeTx{
		now:          testNow,
		fleetCurrent: true,
		topology: domain.FleetTopology{
			FleetID: 7,
			Nodes:   []domain.NodeID{"node-a", "node-b"},
			Bridges: []domain.BridgeRoute{{
				RoutingKey:  "a-to-b",
				EntryNodeID: "node-a",
				ExitNodeID:  "node-b",
				EgressTag:   "exit-b",
			}},
		},
	}
	uc, repo, ids, sealer := newHarness(tx)

	if err := uc.Execute(context.Background(), request(validCommand())); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !repo.committed {
		t.Fatal("команда должна коммититься")
	}

	// Периода у нового customer нет, поэтому его блокировка не запрашивается.
	want := []string{
		"Now", "LockEntitlement", "FleetIsCurrent",
		"LoadTopology", "LoadAccesses", "WritePlan", "AppendAudit",
	}
	if !slices.Equal(tx.calls, want) {
		t.Fatalf("вызовы %v, ожидалось %v", tx.calls, want)
	}

	plan := tx.written
	if plan == nil {
		t.Fatal("план не записан")
	}
	if !plan.Plan.CreateEntitlement || !plan.Plan.OpenNewPeriod {
		t.Fatal("первый Apply обязан создать entitlement и открыть период")
	}
	if plan.CommandNumber != 5 {
		t.Fatalf("CommandNumber = %d, ожидалось 5", plan.CommandNumber)
	}
	if plan.PeriodID == uuid.Nil {
		t.Fatal("новому периоду не выдан идентификатор")
	}

	// link_count = fleet_node_count + bridge_relation_count (§4).
	if len(plan.NewAccesses) != 3 {
		t.Fatalf("создано %d access, ожидалось 3", len(plan.NewAccesses))
	}
	if len(plan.Operations) != 3 {
		t.Fatalf("создано %d операций, ожидалось 3", len(plan.Operations))
	}

	// Каждый новый access получает собственные accounting_id и client_uuid (§4).
	if ids.accountingIDs != 3 || ids.clientUUIDs != 3 || sealer.sealed != 3 {
		t.Fatalf("accounting=%d client_uuid=%d sealed=%d, ожидалось по 3",
			ids.accountingIDs, ids.clientUUIDs, sealer.sealed)
	}

	accountingIDs := make(map[string]struct{})
	for _, access := range plan.NewAccesses {
		if access.AccessID == uuid.Nil {
			t.Fatal("access без идентификатора")
		}
		if access.Credential.IsZero() {
			t.Fatal("access без зашифрованного client_uuid")
		}
		accountingIDs[access.AccountingID] = struct{}{}
	}
	if len(accountingIDs) != 3 {
		t.Fatal("accounting_id обязан быть уникальным на access (§9)")
	}

	// Все операции на создание — EnsureUserPresent первой версии (§9, решение 3).
	for _, op := range plan.Operations {
		if op.DesiredState != domain.DesiredStatePresent {
			t.Fatalf("операция с desired_state %q", op.DesiredState)
		}
		if op.DesiredVersion != 1 {
			t.Fatalf("desired_version = %d, у создания всегда 1", op.DesiredVersion)
		}
		if op.OperationID == uuid.Nil {
			t.Fatal("операция без идентификатора")
		}
	}
}

// Access, родившийся ABSENT (истёкший customer), операции не получает: юзера на
// ноде не было, а отсутствие — состояние Xray по умолчанию (решение 3.2).
func TestExecuteAbsentAtBirthIssuesNoOperation(t *testing.T) {
	// Существующий истёкший customer: повтор его команды с тем же expiry допустим
	// (§5, правило 7), а новая нода fleet даёт ABSENT-access.
	expired := testNow.Add(-time.Hour)
	tx := &fakeTx{
		now:          testNow,
		fleetCurrent: true,
		entitlement: &domain.Entitlement{
			FleetID:           7,
			ExpiresAt:         expired,
			LastCommandNumber: 4,
			DesiredVersion:    2,
		},
		openPeriod: &domain.QuotaPeriod{
			ID:              uuid.New(),
			UsageQuotaBytes: 1 << 30,
			StartedAt:       testNow.Add(-48 * time.Hour),
		},
		topology: domain.FleetTopology{FleetID: 7, Nodes: []domain.NodeID{"node-a"}},
	}
	uc, _, ids, _ := newHarness(tx)

	cmd := validCommand()
	cmd.ExpiresAt = expired // тот же expiry: не renewal

	if err := uc.Execute(context.Background(), request(cmd)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	plan := tx.written
	if plan == nil {
		t.Fatal("план не записан")
	}
	if len(plan.NewAccesses) != 1 {
		t.Fatalf("создано %d access, ожидался 1", len(plan.NewAccesses))
	}
	if got := plan.NewAccesses[0].Spec.DesiredState; got != domain.DesiredStateAbsent {
		t.Fatalf("desired_state = %q, ожидался ABSENT у истёкшего customer", got)
	}
	if len(plan.Operations) != 0 {
		t.Fatalf("создано %d операций, ожидалось 0", len(plan.Operations))
	}
	if ids.operationIDs != 0 {
		t.Fatal("для ABSENT-access не должно выдаваться operation_id")
	}
	// Период не открывается заново, поэтому и его идентификатор не нужен.
	if ids.periodIDs != 0 {
		t.Fatal("тот же expiry не открывает новый период (§5, правило 7)")
	}
	if plan.PeriodID != tx.openPeriod.ID {
		t.Fatal("изменения квоты обязаны писаться в уже открытый период")
	}
}

// Точный повтор принятой команды с бо́льшим номером не меняет целевого состояния:
// операций нет, период не открывается, но номер команды двигается (§5, правило 4).
func TestExecuteAcceptedNoOpStillAdvancesCommandNumber(t *testing.T) {
	periodID := uuid.New()
	accessID := uuid.New()

	tx := &fakeTx{
		now:          testNow,
		fleetCurrent: true,
		entitlement: &domain.Entitlement{
			FleetID:           7,
			ExpiresAt:         testNow.Add(30 * 24 * time.Hour),
			LastCommandNumber: 4,
			DesiredVersion:    3,
		},
		openPeriod: &domain.QuotaPeriod{
			ID:              periodID,
			UsageQuotaBytes: 1 << 30,
			StartedAt:       testNow.Add(-time.Hour),
		},
		nodeUsage: []domain.NodeQuotaUsage{{NodeID: "node-a", TotalBytes: 100}},
		topology:  domain.FleetTopology{FleetID: 7, Nodes: []domain.NodeID{"node-a"}},
		accesses: []domain.Access{{
			ID:               accessID,
			Kind:             domain.AccessKindFreedom,
			LogicalTargetKey: "node-a",
			Generation:       1,
			EntryNodeID:      "node-a",
			EgressKey:        domain.FreedomEgressKey,
			AccountingID:     "u.existing",
			DesiredState:     domain.DesiredStatePresent,
			DesiredVersion:   1,
		}},
	}
	uc, repo, ids, sealer := newHarness(tx)

	if err := uc.Execute(context.Background(), request(validCommand())); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !repo.committed {
		t.Fatal("валидный no-op обязан коммититься")
	}

	// У существующего customer период и расход блокируются до чтения топологии —
	// нормативный порядок §11.1.
	want := []string{
		"Now", "LockEntitlement", "FleetIsCurrent",
		"LockOpenQuotaPeriod", "LockNodeQuotaUsage",
		"LoadTopology", "LoadAccesses", "WritePlan", "AppendAudit",
	}
	if !slices.Equal(tx.calls, want) {
		t.Fatalf("вызовы %v, ожидалось %v", tx.calls, want)
	}

	plan := tx.written
	if plan == nil {
		t.Fatal("план не записан: last_command_number обязан двигаться и на no-op")
	}
	if !plan.Plan.IsNoOp() {
		t.Fatalf("план не пуст: %+v", plan.Plan)
	}
	if plan.CommandNumber != 5 {
		t.Fatalf("CommandNumber = %d, ожидалось 5", plan.CommandNumber)
	}
	if plan.Plan.EntitlementDesiredVersion != 3 {
		t.Fatalf("desired_version = %d, на пустом плане он не растёт (решение 3.5)",
			plan.Plan.EntitlementDesiredVersion)
	}
	if len(plan.Operations) != 0 || len(plan.NewAccesses) != 0 {
		t.Fatal("no-op не создаёт ни операций, ни access")
	}
	if ids.accessIDs+ids.operationIDs+ids.periodIDs != 0 || sealer.sealed != 0 {
		t.Fatal("no-op не должен генерировать ни идентификаторов, ни credentials")
	}
}

// Исчерпание квоты на одной ноде переводит в ABSENT только её access и порождает
// EnsureUserAbsent только для неё; на других нодах доступ сохраняется (§4, §5).
func TestExecuteQuotaDecreaseBlocksOnlyExhaustedNode(t *testing.T) {
	periodID := uuid.New()
	accessA, accessB := uuid.New(), uuid.New()

	tx := &fakeTx{
		now:          testNow,
		fleetCurrent: true,
		entitlement: &domain.Entitlement{
			FleetID:           7,
			ExpiresAt:         testNow.Add(30 * 24 * time.Hour),
			LastCommandNumber: 4,
			DesiredVersion:    3,
		},
		openPeriod: &domain.QuotaPeriod{
			ID:              periodID,
			UsageQuotaBytes: 10 << 30,
			StartedAt:       testNow.Add(-time.Hour),
		},
		nodeUsage: []domain.NodeQuotaUsage{
			{NodeID: "node-a", TotalBytes: 5 << 30}, // выше нового лимита
			{NodeID: "node-b", TotalBytes: 1 << 20}, // ниже
		},
		topology: domain.FleetTopology{FleetID: 7, Nodes: []domain.NodeID{"node-a", "node-b"}},
		accesses: []domain.Access{
			{
				ID: accessA, Kind: domain.AccessKindFreedom, LogicalTargetKey: "node-a",
				Generation: 1, EntryNodeID: "node-a", EgressKey: domain.FreedomEgressKey,
				AccountingID: "u.a", DesiredState: domain.DesiredStatePresent, DesiredVersion: 1,
			},
			{
				ID: accessB, Kind: domain.AccessKindFreedom, LogicalTargetKey: "node-b",
				Generation: 1, EntryNodeID: "node-b", EgressKey: domain.FreedomEgressKey,
				AccountingID: "u.b", DesiredState: domain.DesiredStatePresent, DesiredVersion: 1,
			},
		},
	}
	uc, _, _, _ := newHarness(tx)

	cmd := validCommand()
	cmd.UsageQuotaBytes = 1 << 30 // понижение: node-a уже исчерпала новый лимит

	if err := uc.Execute(context.Background(), request(cmd)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	plan := tx.written
	if plan == nil {
		t.Fatal("план не записан")
	}
	if !plan.Plan.UpdatePeriodQuota {
		t.Fatal("новый лимит обязан записаться в открытый период")
	}
	if len(plan.Operations) != 1 {
		t.Fatalf("создано %d операций, ожидалась 1 (только для исчерпавшей ноды)", len(plan.Operations))
	}

	op := plan.Operations[0]
	if op.NodeID != "node-a" {
		t.Fatalf("операция ушла на ноду %q, ожидалась node-a", op.NodeID)
	}
	if op.AccessID != accessA {
		t.Fatal("операция выпущена не на тот access")
	}
	if op.DesiredState != domain.DesiredStateAbsent {
		t.Fatalf("desired_state = %q, ожидался ABSENT", op.DesiredState)
	}
	if op.DesiredVersion != 2 {
		t.Fatalf("desired_version = %d, ожидалось 2 (прежняя + 1)", op.DesiredVersion)
	}
	if !slices.Equal(plan.Plan.TouchedNodes, []domain.NodeID{"node-a"}) {
		t.Fatalf("TouchedNodes = %v, ожидалось [node-a]", plan.Plan.TouchedNodes)
	}
}

// Отказ CSPRNG или шифрования обязан провалить команду, а не записать access без
// credential.
func TestExecutePropagatesSealError(t *testing.T) {
	tx := &fakeTx{
		now:          testNow,
		fleetCurrent: true,
		topology:     domain.FleetTopology{FleetID: 7, Nodes: []domain.NodeID{"node-a"}},
	}
	repo := &fakeRepo{tx: tx}
	sealFailed := errors.New("шифрование недоступно")
	uc := app.NewApplyCustomerAccess(repo, &countingIDs{}, &stubSealer{err: sealFailed})

	err := uc.Execute(context.Background(), request(validCommand()))
	if !errors.Is(err, sealFailed) {
		t.Fatalf("Execute = %v, ожидался проброс ошибки Seal", err)
	}
	if tx.written != nil {
		t.Fatal("план не должен записываться при отказе шифрования")
	}
	if repo.committed {
		t.Fatal("отказ шифрования обязан откатывать транзакцию")
	}
}

// --- аудит команды customer (§15) ---------------------------------------------

// existingCustomerTx — уже заведённый customer с открытым периодом. Основа для
// решений RENEWAL и QUOTA_CHANGE: оба требуют сохранённой корневой строки.
func existingCustomerTx() *fakeTx {
	return &fakeTx{
		now:          testNow,
		fleetCurrent: true,
		entitlement: &domain.Entitlement{
			FleetID:           7,
			ExpiresAt:         testNow.Add(24 * time.Hour),
			LastCommandNumber: 4,
			DesiredVersion:    2,
		},
		openPeriod: &domain.QuotaPeriod{
			ID:              uuid.New(),
			UsageQuotaBytes: 1 << 30,
			StartedAt:       testNow.Add(-48 * time.Hour),
		},
		topology: domain.FleetTopology{FleetID: 7, Nodes: []domain.NodeID{"node-a"}},
	}
}

// newCustomerTx — customer, которого ещё нет.
func newCustomerTx() *fakeTx {
	return &fakeTx{
		now:          testNow,
		fleetCurrent: true,
		topology:     domain.FleetTopology{FleetID: 7, Nodes: []domain.NodeID{"node-a"}},
	}
}

// applyForAudit прогоняет команду и возвращает записанные события.
func applyForAudit(t *testing.T, tx *fakeTx, cmd domain.ApplyCommand) []app.AuditEvent {
	t.Helper()

	uc, _, _, _ := newHarness(tx)
	if err := uc.Execute(context.Background(), request(cmd)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return tx.audits
}

// TestApplyAuditsDecision — §15 перечисляет «Apply/renewal» раздельно, поэтому
// решение домена попадает в action, а не в метаданные: фильтр по колонке дешевле
// и надёжнее разбора jsonb.
func TestApplyAuditsDecision(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func() (*fakeTx, domain.ApplyCommand)
		want    string
	}{
		{
			name: "первый Apply — создание",
			prepare: func() (*fakeTx, domain.ApplyCommand) {
				return newCustomerTx(), validCommand()
			},
			want: "CUSTOMER_CREATED",
		},
		{
			name: "срок вырос — продление",
			prepare: func() (*fakeTx, domain.ApplyCommand) {
				tx := existingCustomerTx()
				cmd := validCommand()
				cmd.CommandNumber = tx.entitlement.LastCommandNumber + 1
				cmd.ExpiresAt = tx.entitlement.ExpiresAt.Add(30 * 24 * time.Hour)
				return tx, cmd
			},
			want: "CUSTOMER_RENEWED",
		},
		{
			name: "срок тот же — смена квоты",
			prepare: func() (*fakeTx, domain.ApplyCommand) {
				tx := existingCustomerTx()
				cmd := validCommand()
				cmd.CommandNumber = tx.entitlement.LastCommandNumber + 1
				cmd.ExpiresAt = tx.entitlement.ExpiresAt
				cmd.UsageQuotaBytes = 9 << 30
				return tx, cmd
			},
			want: "CUSTOMER_QUOTA_CHANGED",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, cmd := tc.prepare()

			audits := applyForAudit(t, tx, cmd)
			if len(audits) != 1 {
				t.Fatalf("записей аудита %d, ожидалась 1", len(audits))
			}
			if audits[0].Action != tc.want {
				t.Errorf("action %q, ожидался %q", audits[0].Action, tc.want)
			}
		})
	}
}

// TestApplyAuditsEmptyPlan — команда, ничего не изменившая, всё равно принята:
// last_command_number сдвинулся, и «backend принял команду N» — ровно тот факт,
// который потом сверяют с product-сервисом.
func TestApplyAuditsEmptyPlan(t *testing.T) {
	tx := existingCustomerTx()
	// Access под текущую топологию уже есть, поэтому создавать нечего.
	tx.accesses = []domain.Access{{
		ID:               uuid.New(),
		Kind:             domain.AccessKindFreedom,
		LogicalTargetKey: "node-a",
		Generation:       1,
		EntryNodeID:      "node-a",
		EgressKey:        domain.FreedomEgressKey,
		AccountingID:     "u.existing",
		DesiredState:     domain.DesiredStatePresent,
		DesiredVersion:   1,
	}}

	cmd := validCommand()
	cmd.CommandNumber = tx.entitlement.LastCommandNumber + 1
	cmd.ExpiresAt = tx.entitlement.ExpiresAt
	cmd.UsageQuotaBytes = tx.openPeriod.UsageQuotaBytes // и срок, и квота прежние

	audits := applyForAudit(t, tx, cmd)
	if len(audits) != 1 {
		t.Fatalf("записей аудита %d, ожидалась 1", len(audits))
	}
	if got := audits[0].Metadata["created_access"]; got != 0 {
		t.Errorf("created_access %v, ожидался 0: план обязан быть пустым", got)
	}
}

// TestApplyAuditCarriesCaller — actor и request_id связывают запись с
// mTLS-идентичностью вызывающего и логами того же запроса (§15).
func TestApplyAuditCarriesCaller(t *testing.T) {
	audits := applyForAudit(t, newCustomerTx(), validCommand())
	if len(audits) != 1 {
		t.Fatalf("записей аудита %d, ожидалась 1", len(audits))
	}

	event := audits[0]
	if event.ActorID != testActor || event.RequestID != testRequestID {
		t.Errorf("actor %q request %q, ожидались %q и %q",
			event.ActorID, event.RequestID, testActor, testRequestID)
	}
	// customer_id разрешён §15 именно в audit records и живёт в target_id.
	if event.TargetID != validCommand().CustomerID {
		t.Errorf("target_id %q, ожидался %q", event.TargetID, validCommand().CustomerID)
	}
	if event.Outcome != "ACCEPTED" {
		t.Errorf("outcome %q, ожидался ACCEPTED", event.Outcome)
	}
}

// TestApplyAuditMetadataCarriesNoSecrets — §15 запрещает секреты в журнале.
//
// Проверка идёт по ЗНАЧЕНИЯМ, а не по именам ключей: accounting_id и client_uuid
// опасны именно как значения, и попасть туда они могут под любым именем.
func TestApplyAuditMetadataCarriesNoSecrets(t *testing.T) {
	tx := newCustomerTx()
	audits := applyForAudit(t, tx, validCommand())

	if tx.written == nil || len(tx.written.NewAccesses) == 0 {
		t.Fatal("подготовка: план не создал ни одного access, проверять нечего")
	}

	rendered := fmt.Sprint(audits[0].Metadata)
	for _, access := range tx.written.NewAccesses {
		if strings.Contains(rendered, access.AccountingID) {
			t.Errorf("метаданные несут accounting_id: %s", rendered)
		}
	}
	for _, forbidden := range []string{"client_uuid", "credential", "uri"} {
		if strings.Contains(strings.ToLower(rendered), forbidden) {
			t.Errorf("метаданные несут %q: %s", forbidden, rendered)
		}
	}
}

// TestApplyStaleCommandWritesNoAudit — устаревшая команда не имеет side effects
// (§5, правило 2), и запись о ней означала бы изменение, которого не было.
// Повтор доставки иначе плодил бы дубликаты одного и того же no-op.
func TestApplyStaleCommandWritesNoAudit(t *testing.T) {
	tx := existingCustomerTx()
	cmd := validCommand()
	cmd.CommandNumber = tx.entitlement.LastCommandNumber // не больше сохранённого

	uc, _, _, _ := newHarness(tx)
	if err := uc.Execute(context.Background(), request(cmd)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(tx.audits) != 0 {
		t.Errorf("устаревшая команда записала аудит: %+v", tx.audits)
	}
}

// TestApplyRejectedCommandWritesNoAudit — отклонённая команда до записи журнала
// не доходит. Это предел аудита v1: следа у отказа не остаётся, зато журнал не
// может утверждать про изменения, которых нет.
//
// Здесь проверяется именно «не дошла»: фейковая транзакция ничего не откатывает,
// поэтому сам откат журнала вместе с командой проверяется интеграционно
// (TestIntegrationApplyAuditRollsBackWithCommand).
func TestApplyRejectedCommandWritesNoAudit(t *testing.T) {
	tx := &fakeTx{now: testNow, fleetCurrent: false}

	uc, repo, _, _ := newHarness(tx)
	if err := uc.Execute(context.Background(), request(validCommand())); !errors.Is(err, domain.ErrFleetNotFound) {
		t.Fatalf("Execute = %v, ожидалась ErrFleetNotFound", err)
	}

	if repo.committed {
		t.Fatal("отклонённая команда обязана откатывать транзакцию")
	}
	if len(tx.audits) != 0 {
		t.Errorf("отклонённая команда записала аудит: %+v", tx.audits)
	}
}
