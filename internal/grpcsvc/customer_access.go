// Package grpcsvc — транспортный адаптер внешнего gRPC API.
//
// Пакет намеренно тонкий: он переводит protobuf-сообщения в доменные команды,
// вызывает use case и отображает ошибки в gRPC-статусы. Правил валидации здесь нет —
// валидация живёт в domain.ValidateApplyCommand, порядок команд и транзакция в
// app.ApplyCustomerAccess. Дублировать их на транспорте нельзя: два источника
// одного правила со временем расходятся.
//
// Авторизация — mTLS и роли customer-access-writer / customer-access-reader —
// хендлерам не видна: её выполняет interceptor до входа в метод.
package grpcsvc

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	customerv1 "github.com/RomanRyabinkin/SpiritVPN/internal/gen/spiritvpn/customer/v1"
)

// Запрет кеширования ответа с URI. gRPC не знает про HTTP-семантику,
// поэтому это обычная метадата ответа — тот самый «эквивалент Cache-Control:
// no-store на поддерживающем metadata transport». Ключи
// метадаты gRPC регистронезависимы и хранятся в нижнем регистре.
const (
	headerCacheControl  = "cache-control"
	cacheControlNoStore = "no-store"
)

// applyCustomerAccess — use case, который обслуживает метод.
//
// Узкий интерфейс вместо *app.ApplyCustomerAccess: транспорту нужен ровно один
// вызов, а тест хендлера не должен тащить за собой репозиторий, sealer и
// генератор идентификаторов.
type applyCustomerAccess interface {
	Execute(ctx context.Context, cmd app.ApplyCustomerCommand) error
}

// getCustomerAccessLinks — use case запроса ссылок.
type getCustomerAccessLinks interface {
	Execute(ctx context.Context, customerID string) ([]app.CustomerAccessLink, error)
}

// listAvailableNodes — use case публичного каталога нод.
type listAvailableNodes interface {
	Execute(ctx context.Context) ([]app.AvailableFleet, error)
}

type setCustomerAccessState interface {
	Execute(context.Context, app.SetCustomerAccessStateCommand) error
}

type deleteCustomerAccess interface {
	Execute(context.Context, app.DeleteCustomerAccessCommand) (domain.CustomerDeletionState, error)
}

type CustomerAccessAdministration struct {
	State  setCustomerAccessState
	Delete deleteCustomerAccess
}

// CustomerAccessServer реализует CustomerAccessService.
type CustomerAccessServer struct {
	// Встраивание по значению обязательно: сгенерированный
	// RegisterCustomerAccessServiceServer паникует на встраивании указателем.
	customerv1.UnimplementedCustomerAccessServiceServer

	apply applyCustomerAccess
	links getCustomerAccessLinks
	nodes listAvailableNodes
	admin CustomerAccessAdministration
}

// NewCustomerAccessServer собирает транспорт поверх use case'ов.
func NewCustomerAccessServer(
	apply applyCustomerAccess,
	links getCustomerAccessLinks,
	nodes listAvailableNodes,
	administration ...CustomerAccessAdministration,
) *CustomerAccessServer {
	server := &CustomerAccessServer{apply: apply, links: links, nodes: nodes}
	if len(administration) > 0 {
		server.admin = administration[0]
	}
	return server
}

// ApplyCustomerAccess принимает одну команду product-сервиса.
//
// Пустой ответ означает, что desired state и durable agent operations
// зафиксированы в PostgreSQL, но НЕ то, что агенты их уже применили. Он же
// возвращается на поглощённый реордер или повтор команды:
// снаружи принятие и идемпотентный no-op неразличимы.
func (s *CustomerAccessServer) ApplyCustomerAccess(
	ctx context.Context,
	req *customerv1.ApplyCustomerAccessRequest,
) (*customerv1.ApplyCustomerAccessResponse, error) {
	// Идентичность вызывающего и request_id уезжают в audit_events. Роль
	// уже проверена interceptor'ом; здесь берётся сама идентичность, потому что
	// журналу нужен конкретный вызывающий, а не его класс.
	if err := s.apply.Execute(ctx, app.ApplyCustomerCommand{
		Command:   applyCommandFrom(req),
		Actor:     peerIdentity(ctx),
		RequestID: RequestIDFromContext(ctx),
	}); err != nil {
		return nil, statusFromError(ctx, err)
	}

	return &customerv1.ApplyCustomerAccessResponse{}, nil
}

// GetCustomerAccessLinks возвращает все текущие ссылки customer.
//
// Ответ не постраничный и не кешируемый. Частично готовый fleet — штатный исход:
// готовые URI отдаются вместе с состояниями остальных access, retired наружу не
// уходят, а цели, которых нет в текущем manifest, в выборку не попадают.
func (s *CustomerAccessServer) GetCustomerAccessLinks(
	ctx context.Context,
	req *customerv1.GetCustomerAccessLinksRequest,
) (*customerv1.GetCustomerAccessLinksResponse, error) {
	// Заголовок ставится до вызова use case, а не после: он нужен на любом ответе
	// с URI, и привязка к успеху полагалась бы на то, что между проверкой и
	// записью ничего не изменится.
	if err := grpc.SetHeader(ctx, metadata.Pairs(headerCacheControl, cacheControlNoStore)); err != nil {
		return nil, statusFromError(ctx, err)
	}

	links, err := s.links.Execute(ctx, req.GetCustomerId())
	if err != nil {
		return nil, statusFromError(ctx, err)
	}

	response := &customerv1.GetCustomerAccessLinksResponse{
		Links: make([]*customerv1.CustomerAccessLink, 0, len(links)),
	}
	for _, link := range links {
		converted, convErr := linkTo(link)
		if convErr != nil {
			return nil, statusFromError(ctx, convErr)
		}
		response.Links = append(response.Links, converted)
	}

	return response, nil
}

// ListAvailableNodes возвращает актуальные ноды manifest по fleets.
//
// В ответе нет credentials, поэтому cache-control: no-store, обязательный для
// GetCustomerAccessLinks, здесь не выставляется.
func (s *CustomerAccessServer) ListAvailableNodes(
	ctx context.Context,
	_ *customerv1.ListAvailableNodesRequest,
) (*customerv1.ListAvailableNodesResponse, error) {
	fleets, err := s.nodes.Execute(ctx)
	if err != nil {
		return nil, statusFromError(ctx, err)
	}

	response := &customerv1.ListAvailableNodesResponse{
		Fleets: make([]*customerv1.AvailableFleet, 0, len(fleets)),
	}
	for _, fleet := range fleets {
		converted := &customerv1.AvailableFleet{
			VpnFleetId: fleet.FleetID,
			Nodes:      make([]*customerv1.AvailableNode, 0, len(fleet.Nodes)),
		}
		for _, node := range fleet.Nodes {
			converted.Nodes = append(converted.Nodes, &customerv1.AvailableNode{
				NodeId:      node.NodeID,
				DisplayName: node.DisplayName,
			})
		}
		response.Fleets = append(response.Fleets, converted)
	}

	return response, nil
}

func (s *CustomerAccessServer) SetCustomerAccessState(
	ctx context.Context,
	req *customerv1.SetCustomerAccessStateRequest,
) (*customerv1.SetCustomerAccessStateResponse, error) {
	target, ok := administrativeStates[req.GetState()]
	if !ok {
		return nil, statusFromError(ctx, domain.ErrAdministrativeStateInvalid)
	}
	if s.admin.State == nil {
		return nil, statusFromError(ctx, fmt.Errorf("admin use case не настроен"))
	}
	if err := s.admin.State.Execute(ctx, app.SetCustomerAccessStateCommand{
		Command: domain.AdministrativeCommand{
			CustomerID: req.GetCustomerId(), Target: target, CommandNumber: req.GetCommandNumber(),
		},
		Actor: peerIdentity(ctx), RequestID: RequestIDFromContext(ctx),
	}); err != nil {
		return nil, statusFromError(ctx, err)
	}
	return &customerv1.SetCustomerAccessStateResponse{}, nil
}

func (s *CustomerAccessServer) DeleteCustomerAccess(
	ctx context.Context,
	req *customerv1.DeleteCustomerAccessRequest,
) (*customerv1.DeleteCustomerAccessResponse, error) {
	if s.admin.Delete == nil {
		return nil, statusFromError(ctx, fmt.Errorf("delete use case не настроен"))
	}
	state, err := s.admin.Delete.Execute(ctx, app.DeleteCustomerAccessCommand{
		Command: domain.DeleteCommand{CustomerID: req.GetCustomerId(), CommandNumber: req.GetCommandNumber()},
		Actor:   peerIdentity(ctx), RequestID: RequestIDFromContext(ctx),
	})
	if err != nil {
		return nil, statusFromError(ctx, err)
	}
	converted, ok := deletionStates[state]
	if !ok {
		return nil, statusFromError(ctx, fmt.Errorf("неизвестное состояние удаления %q", state))
	}
	return &customerv1.DeleteCustomerAccessResponse{State: converted}, nil
}

// Словари доменных значений в протокольные энумы.
//
// Именно словари, а не switch с default: промах по ключу обязан стать ошибкой.
// Значение вне словаря означает, что домен и proto разошлись, и молчаливый
// UNSPECIFIED увёз бы это расхождение клиенту под видом валидного ответа.
var (
	accessKinds = map[domain.AccessKind]customerv1.AccessKind{
		domain.AccessKindFreedom: customerv1.AccessKind_ACCESS_KIND_FREEDOM,
		domain.AccessKindBridge:  customerv1.AccessKind_ACCESS_KIND_BRIDGE,
	}

	linkStates = map[domain.LinkState]customerv1.AccessLinkState{
		domain.LinkStatePending: customerv1.AccessLinkState_ACCESS_LINK_STATE_PENDING,
		domain.LinkStateReady:   customerv1.AccessLinkState_ACCESS_LINK_STATE_READY,
		domain.LinkStateBlocked: customerv1.AccessLinkState_ACCESS_LINK_STATE_BLOCKED,
		domain.LinkStateFailed:  customerv1.AccessLinkState_ACCESS_LINK_STATE_FAILED,
	}

	blockReasons = map[domain.BlockReason]customerv1.AccessBlockReason{
		domain.BlockReasonTimeExpired:           customerv1.AccessBlockReason_ACCESS_BLOCK_REASON_TIME_EXPIRED,
		domain.BlockReasonTrafficQuotaExhausted: customerv1.AccessBlockReason_ACCESS_BLOCK_REASON_TRAFFIC_QUOTA_EXHAUSTED,
		domain.BlockReasonAdministrative:        customerv1.AccessBlockReason_ACCESS_BLOCK_REASON_ADMINISTRATIVE_BLOCK,
		domain.BlockReasonDeleting:              customerv1.AccessBlockReason_ACCESS_BLOCK_REASON_DELETION_IN_PROGRESS,
	}

	administrativeStates = map[customerv1.AdministrativeAccessState]domain.CustomerLifecycle{
		customerv1.AdministrativeAccessState_ADMINISTRATIVE_ACCESS_STATE_ACTIVE:  domain.CustomerLifecycleActive,
		customerv1.AdministrativeAccessState_ADMINISTRATIVE_ACCESS_STATE_BLOCKED: domain.CustomerLifecycleBlocked,
	}

	deletionStates = map[domain.CustomerDeletionState]customerv1.CustomerDeletionState{
		domain.CustomerDeletionPending:   customerv1.CustomerDeletionState_CUSTOMER_DELETION_STATE_PENDING,
		domain.CustomerDeletionCompleted: customerv1.CustomerDeletionState_CUSTOMER_DELETION_STATE_COMPLETED,
	}
)

// linkTo переводит одну ссылку в protobuf.
//
// block_reason и uri — условные optional-поля: причина присутствует только у
// BLOCKED, URI — только у READY. Quota-поля, напротив, присутствуют при любом
// состоянии ссылки и относятся к её входной ноде.
func linkTo(link app.CustomerAccessLink) (*customerv1.CustomerAccessLink, error) {
	kind, ok := accessKinds[link.Kind]
	if !ok {
		return nil, fmt.Errorf("grpcsvc: неизвестный kind %q", link.Kind)
	}

	state, ok := linkStates[link.Status.State]
	if !ok {
		return nil, fmt.Errorf("grpcsvc: неизвестное состояние ссылки %q", link.Status.State)
	}

	quota := link.UsageQuotaBytes
	consumed := link.ConsumedBytes
	converted := &customerv1.CustomerAccessLink{
		Kind:            kind,
		State:           state,
		UsageQuotaBytes: &quota,
		ConsumedBytes:   &consumed,
	}

	if link.Status.State == domain.LinkStateBlocked {
		reason, known := blockReasons[link.Status.Reason]
		if !known {
			return nil, fmt.Errorf("grpcsvc: неизвестная причина блокировки %q", link.Status.Reason)
		}
		converted.BlockReason = &reason
	}

	if link.Status.State == domain.LinkStateReady {
		converted.Uri = &link.URI
	}

	return converted, nil
}

// applyCommandFrom переводит запрос в доменную команду.
//
// Здесь и только здесь закрепляется секундная точность expires_at.
// Домен её не проверяет: среди правил валидации точности нет, её гарантирует
// тип поля expires_at_epoch_sec. Но timestamptz хранит
// микросекунды, поэтому время с наносекундами прочиталось бы из базы строго
// меньшим, и точный повтор команды классифицировался бы как renewal со сбросом
// счётчиков трафика.
func applyCommandFrom(req *customerv1.ApplyCustomerAccessRequest) domain.ApplyCommand {
	return domain.ApplyCommand{
		CustomerID:      req.GetCustomerId(),
		FleetID:         req.GetVpnFleetId(),
		UsageQuotaBytes: req.GetUsageQuotaBytes(),
		ExpiresAt:       time.Unix(req.GetExpiresAtEpochSec(), 0).UTC(),
		CommandNumber:   req.GetCommandNumber(),
	}
}
