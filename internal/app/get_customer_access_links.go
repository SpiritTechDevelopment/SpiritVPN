package app

import (
	"context"

	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
)

// GetCustomerAccessLinks — use case запроса GetCustomerAccessLinks.
//
// Read-only: ни одной записи, ни одного row lock. Готовая URI не хранится и
// собирается заново на каждый ответ из credential и текущей проекции manifest,
// поэтому смена endpoint или REALITY-параметров ноды подхватывается
// следующим же запросом без единой миграции данных.
type GetCustomerAccessLinks struct {
	Repo   LinksRepository
	Sealer CredentialSealer
}

// NewGetCustomerAccessLinks собирает use case из портов.
func NewGetCustomerAccessLinks(repo LinksRepository, sealer CredentialSealer) *GetCustomerAccessLinks {
	return &GetCustomerAccessLinks{Repo: repo, Sealer: sealer}
}

// CustomerAccessLink — одна ссылка в ответе.
type CustomerAccessLink struct {
	Kind   domain.AccessKind
	Status domain.LinkStatus
	// URI непуста только при READY. Внутри неё открытый client_uuid —
	// см. предупреждение у BuildVLESSURI.
	URI string
}

// Execute возвращает все текущие ссылки customer одним ответом без пагинации.
//
// Частично готовый fleet — штатный исход: готовые ссылки отдаются вместе с
// состояниями остальных, и недоступность одной ноды не скрывает работающие
// ссылки других.
func (uc *GetCustomerAccessLinks) Execute(ctx context.Context, customerID string) ([]CustomerAccessLink, error) {
	if err := domain.ValidateCustomerID(customerID); err != nil {
		return nil, err
	}

	snapshot, err := uc.Repo.LoadCustomerLinks(ctx, customerID)
	if err != nil {
		return nil, err
	}

	links := make([]CustomerAccessLink, 0, len(snapshot.Accesses))
	for _, access := range snapshot.Accesses {
		links = append(links, uc.link(snapshot, access))
	}
	return links, nil
}

// link выводит состояние одной ссылки и, только для READY, собирает URI.
//
// Расшифровка выполняется исключительно на этой ветке: у заблокированной или
// недоставленной ссылки URI всё равно не будет, и вносить ради неё открытый
// секрет в память не нужно.
func (uc *GetCustomerAccessLinks) link(snapshot CustomerLinks, access AccessLinkSource) CustomerAccessLink {
	status := domain.LinkStatusOf(domain.LinkInput{
		Now:            snapshot.Now,
		ExpiresAt:      snapshot.ExpiresAt,
		QuotaExhausted: access.QuotaExhausted,
		DesiredState:   access.DesiredState,
		ApplyState:     access.ApplyState,
		EntryUsable:    access.Entry.Usable(),
	})

	link := CustomerAccessLink{Kind: access.Kind, Status: status}
	if status.State != domain.LinkStateReady {
		return link
	}

	clientUUID, err := uc.Sealer.Open(access.Credential)
	if err != nil {
		// Тот же выбор, что и у непригодной входной ноды: нерабочий
		// credential ломает свою ссылку, а не весь ответ. Причина отказа наружу
		// не уходит: она инфраструктурная и видна в credential_open_errors_total.
		return CustomerAccessLink{Kind: access.Kind, Status: domain.LinkStatus{State: domain.LinkStateFailed}}
	}

	link.URI = BuildVLESSURI(clientUUID, access.Entry, displayNameFor(access))
	return link
}

// displayNameFor выбирает фрагмент URI: для FREEDOM имя ноды, для BRIDGE имя
// связи. Развилка идёт по kind, а не по непустоте имени: пустое
// display_name связи — это её имя, а не признак FREEDOM.
func displayNameFor(access AccessLinkSource) string {
	if access.Kind == domain.AccessKindBridge {
		return access.BridgeDisplayName
	}
	return access.Entry.DisplayName
}
