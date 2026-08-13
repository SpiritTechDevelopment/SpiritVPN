package app_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
)

// Тесты read-пути проверяют то, чего не видно ни в домене, ни в SQL: что
// открытый client_uuid извлекается РОВНО для готовых ссылок. Домен состояний не
// расшифровывает, запрос отдаёт только ciphertext, и решение «открывать или нет»
// принимает единственно use case.

var (
	linksNow     = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	linksExpires = linksNow.Add(24 * time.Hour)
)

// fakeLinksRepo отдаёт заранее подготовленный снимок.
type fakeLinksRepo struct {
	calls      int
	customerID string
	snapshot   app.CustomerLinks
	err        error
}

func (r *fakeLinksRepo) LoadCustomerLinks(_ context.Context, customerID string) (app.CustomerLinks, error) {
	r.calls++
	r.customerID = customerID
	return r.snapshot, r.err
}

// countingSealer считает вызовы Open: именно их число и есть предмет проверки.
type countingSealer struct {
	opened int
	err    error
	value  uuid.UUID
}

func (s *countingSealer) Seal(crypto.ClientUUID) (crypto.SealedCredential, error) {
	return crypto.SealedCredential{}, errors.New("не используется")
}

func (s *countingSealer) Open(crypto.SealedCredential) (crypto.ClientUUID, error) {
	s.opened++
	if s.err != nil {
		return crypto.ClientUUID{}, s.err
	}
	return crypto.NewClientUUID(s.value), nil
}

func (s *countingSealer) KeyID() string { return "test" }

// readySource — access, из которого получается READY-ссылка.
func readySource() app.AccessLinkSource {
	return app.AccessLinkSource{
		Kind:         domain.AccessKindFreedom,
		DesiredState: domain.DesiredStatePresent,
		ApplyState:   domain.ApplyStateApplied,
		Entry:        testNode(),
		Credential:   crypto.SealedCredential{KeyID: "test", Blob: make([]byte, crypto.SealedBlobSize)},
	}
}

func newLinksHarness(sources ...app.AccessLinkSource) (*app.GetCustomerAccessLinks, *fakeLinksRepo, *countingSealer) {
	repo := &fakeLinksRepo{snapshot: app.CustomerLinks{
		Now:       linksNow,
		ExpiresAt: linksExpires,
		Accesses:  sources,
	}}
	sealer := &countingSealer{value: testClientUUID.Reveal()}
	return app.NewGetCustomerAccessLinks(repo, sealer), repo, sealer
}

// TestExecuteReturnsURIOnlyForReady — поле uri присутствует только у READY.
// Заодно проверяется, что расшифровка выполняется ровно один раз, по числу
// готовых ссылок, а не по числу access.
func TestExecuteReturnsURIOnlyForReady(t *testing.T) {
	blocked := readySource()
	blocked.QuotaExhausted = true

	pending := readySource()
	pending.ApplyState = domain.ApplyStatePending

	failed := readySource()
	failed.ApplyState = domain.ApplyStateFailed

	uc, _, sealer := newLinksHarness(readySource(), blocked, pending, failed)

	links, err := uc.Execute(context.Background(), "cust-1")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(links) != 4 {
		t.Fatalf("ссылок %d, ожидалось 4", len(links))
	}

	if links[0].Status.State != domain.LinkStateReady || links[0].URI == "" {
		t.Fatalf("первая ссылка %+v, ожидалась READY с URI", links[0])
	}
	for _, link := range links[1:] {
		if link.URI != "" {
			t.Errorf("состояние %s несёт URI %q", link.Status.State, link.URI)
		}
	}

	if sealer.opened != 1 {
		t.Fatalf("Open вызван %d раз, ожидался 1: расшифровка только для готовых ссылок", sealer.opened)
	}
}

// TestExecuteNeverOpensBlockedCredential — заблокированный customer не должен
// приводить к появлению открытого секрета в памяти вовсе.
func TestExecuteNeverOpensBlockedCredential(t *testing.T) {
	uc, repo, sealer := newLinksHarness(readySource(), readySource())
	repo.snapshot.ExpiresAt = linksNow.Add(-time.Hour)

	links, err := uc.Execute(context.Background(), "cust-1")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	for _, link := range links {
		want := domain.LinkStatus{State: domain.LinkStateBlocked, Reason: domain.BlockReasonTimeExpired}
		if link.Status != want {
			t.Errorf("состояние %+v, ожидалось %+v", link.Status, want)
		}
	}
	if sealer.opened != 0 {
		t.Fatalf("Open вызван %d раз у истёкшего customer", sealer.opened)
	}
}

// TestExecuteFragmentPerKind — фрагмент FREEDOM берётся из имени ноды,
// BRIDGE — из имени связи. Развилка идёт по kind, поэтому проверяются оба.
func TestExecuteFragmentPerKind(t *testing.T) {
	freedom := readySource()
	freedom.Entry.DisplayName = "Netherlands"
	freedom.BridgeDisplayName = "не должно попасть в URI"

	bridge := readySource()
	bridge.Kind = domain.AccessKindBridge
	bridge.Entry.DisplayName = "Netherlands"
	bridge.BridgeDisplayName = "Netherlands via Germany"

	uc, _, _ := newLinksHarness(freedom, bridge)

	links, err := uc.Execute(context.Background(), "cust-1")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	want := []string{"Netherlands", "Netherlands via Germany"}
	for i, link := range links {
		parsed, parseErr := url.Parse(link.URI)
		if parseErr != nil {
			t.Fatalf("URI %q не разбирается: %v", link.URI, parseErr)
		}
		if parsed.Fragment != want[i] {
			t.Errorf("фрагмент %q, ожидался %q", parsed.Fragment, want[i])
		}
	}
}

// TestExecuteEmptyBridgeNameStaysEmpty — пустое display_name связи это её имя, а
// не признак FREEDOM: подставлять вместо него имя входной ноды нельзя, иначе
// BRIDGE и FREEDOM на одной ноде станут в списке неразличимы.
func TestExecuteEmptyBridgeNameStaysEmpty(t *testing.T) {
	bridge := readySource()
	bridge.Kind = domain.AccessKindBridge
	bridge.Entry.DisplayName = "Netherlands"
	bridge.BridgeDisplayName = ""

	uc, _, _ := newLinksHarness(bridge)

	links, err := uc.Execute(context.Background(), "cust-1")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(links[0].URI, "#") {
		t.Fatalf("URI %q несёт фрагмент, ожидался пустой", links[0].URI)
	}
}

// TestExecuteDegradesUnusableNode — рассогласованная проекция ломает
// свою ссылку, а не весь ответ, и расшифровки по ней не происходит.
func TestExecuteDegradesUnusableNode(t *testing.T) {
	broken := readySource()
	broken.Entry.Address = ""

	uc, _, sealer := newLinksHarness(broken, readySource())

	links, err := uc.Execute(context.Background(), "cust-1")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if links[0].Status.State != domain.LinkStateFailed {
		t.Errorf("состояние %s, ожидалось FAILED", links[0].Status.State)
	}
	if links[1].Status.State != domain.LinkStateReady || links[1].URI == "" {
		t.Errorf("рабочая ссылка пострадала от соседней: %+v", links[1])
	}
	if sealer.opened != 1 {
		t.Fatalf("Open вызван %d раз, ожидался 1", sealer.opened)
	}
}

// TestExecuteDegradesUnopenableCredential — нерабочий credential тоже ломает
// только свою ссылку. Причина наружу не уходит: она инфраструктурная.
func TestExecuteDegradesUnopenableCredential(t *testing.T) {
	uc, _, sealer := newLinksHarness(readySource())
	sealer.err = crypto.ErrCiphertextInvalid

	links, err := uc.Execute(context.Background(), "cust-1")
	if err != nil {
		t.Fatalf("Execute вернул ошибку вместо FAILED-ссылки: %v", err)
	}
	if links[0].Status.State != domain.LinkStateFailed {
		t.Fatalf("состояние %s, ожидалось FAILED", links[0].Status.State)
	}
	if links[0].URI != "" {
		t.Fatalf("URI %q при нерасшифрованном credential", links[0].URI)
	}
}

// TestExecuteValidatesCustomerID — пустой customer_id невалиден, и до базы
// такой запрос доходить не должен, иначе пустая строка стала бы поиском.
func TestExecuteValidatesCustomerID(t *testing.T) {
	uc, repo, _ := newLinksHarness()

	_, err := uc.Execute(context.Background(), "")
	if !errors.Is(err, domain.ErrCustomerIDInvalid) {
		t.Fatalf("ошибка %v, ожидалась ErrCustomerIDInvalid", err)
	}
	if repo.calls != 0 {
		t.Fatalf("репозиторий вызван %d раз при невалидном customer_id", repo.calls)
	}
}

// TestExecutePropagatesRepositoryError — ErrCustomerNotFound доезжает до
// транспорта нетронутым: именно из него получается NOT_FOUND.
func TestExecutePropagatesRepositoryError(t *testing.T) {
	uc, repo, _ := newLinksHarness()
	repo.err = domain.ErrCustomerNotFound

	if _, err := uc.Execute(context.Background(), "cust-1"); !errors.Is(err, domain.ErrCustomerNotFound) {
		t.Fatalf("ошибка %v, ожидалась ErrCustomerNotFound", err)
	}
}

// TestExecuteEmptyFleetReturnsEmptyList — fleet может временно не содержать нод,
// и это не ошибка: у существующего customer просто нет ссылок.
func TestExecuteEmptyFleetReturnsEmptyList(t *testing.T) {
	uc, repo, _ := newLinksHarness()

	links, err := uc.Execute(context.Background(), "cust-1")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("ссылок %d, ожидалось 0", len(links))
	}
	if repo.customerID != "cust-1" {
		t.Fatalf("customer_id %q, ожидался cust-1", repo.customerID)
	}
}
