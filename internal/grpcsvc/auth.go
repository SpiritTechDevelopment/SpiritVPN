package grpcsvc

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"

	customerv1 "github.com/RomanRyabinkin/SpiritVPN/internal/gen/spiritvpn/customer/v1"
)

// Role — роль вызывающего сервиса (§14).
type Role string

const (
	RoleCustomerAccessWriter Role = "customer-access-writer"
	RoleCustomerAccessReader Role = "customer-access-reader"
)

// methodRoles — роль, требуемая каждым методом (решение 14).
//
// Таблица работает deny by default: метода здесь нет — значит он запрещён всем.
// Это её главное свойство и оно важнее содержимого. Следующие срезы добавят и
// методы, и сервисы; тот, кто забудет строчку здесь, получит PERMISSION_DENIED
// на первом же вызове вместо метода, молча открытого всему миру.
//
// Иерархии ролей нет: writer НЕ подразумевает reader. §14 перечисляет их как
// отдельные роли, а здесь это ещё и по существу — чтение отдаёт VLESS URI с
// client_uuid внутри, то есть сами credentials (§5, §8). Сервис, которому нужно
// и то и другое, перечисляется в обоих списках конфигурации явно.
var methodRoles = map[string]Role{
	customerv1.CustomerAccessService_ApplyCustomerAccess_FullMethodName:    RoleCustomerAccessWriter,
	customerv1.CustomerAccessService_GetCustomerAccessLinks_FullMethodName: RoleCustomerAccessReader,
}

// Authorizer решает, что позволено идентичности из клиентского сертификата.
//
// mTLS отвечает только на вопрос «кто подключился»: для TLS все сертификаты,
// подписанные нашим CA, равны, а подписаны им и product-сервис, и
// infrastructure pipeline. Ответ на «что этому клиенту можно» — здесь.
type Authorizer struct {
	identities map[Role]map[string]struct{}
}

// NewAuthorizer строит авторизатор из списков идентичностей по ролям.
func NewAuthorizer(roles map[Role][]string) *Authorizer {
	authorizer := &Authorizer{identities: make(map[Role]map[string]struct{}, len(roles))}

	for role, ids := range roles {
		set := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			// Пустая идентичность отбрасывается второй раз, хотя config это уже
			// сделал. Дублирование намеренно: совпадение пустого с пустым
			// раздаёт роль любому валидному сертификату, и цена ошибки здесь
			// несоизмерима с ценой лишней строки (решение 13).
			if id == "" {
				continue
			}
			set[id] = struct{}{}
		}
		authorizer.identities[role] = set
	}

	return authorizer
}

// UnaryInterceptor проверяет право вызывающего на метод (§14).
func (a *Authorizer) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		if err := a.authorize(ctx, info.FullMethod); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

func (a *Authorizer) authorize(ctx context.Context, method string) error {
	role, known := methodRoles[method]
	if !known {
		return newStatusError(codes.PermissionDenied, codePermissionDenied, "метод недоступен")
	}

	ids := peerIdentities(ctx)
	if len(ids) == 0 {
		// Сюда попадают и вызовы без TLS, и сертификаты без пригодного SAN.
		// Различать их наружу не нужно: обе ситуации означают, что вызывающий не
		// доказал, кто он.
		return newStatusError(codes.Unauthenticated, codeUnauthenticated, "требуется клиентский сертификат с идентичностью")
	}

	allowed := a.identities[role]
	for _, id := range ids {
		if _, ok := allowed[id]; ok {
			return nil
		}
	}

	// Требуемая роль наружу не называется: подсказывать вызывающему, какого
	// именно права ему не хватает, незачем.
	return newStatusError(codes.PermissionDenied, codePermissionDenied, "недостаточно прав")
}

// peerIdentities возвращает идентичности из проверенного сертификата клиента.
//
// Берётся VerifiedChains, а не PeerCertificates: первое прошло проверку цепочкой
// до нашего CA, второе — то, что клиент про себя заявил. При
// tls.RequireAndVerifyClientCert это на практике один и тот же лист, но привычка
// должна быть правильной.
//
// CN не используется (решение 13): как идентичность он давно отставлен — Go
// выкинул CN-fallback из проверки имени хоста ещё в 1.15, — уникальности не
// гарантирует, а у cert-manager и step-ca бывает пустым. Пустая идентичность,
// встретив пустой элемент в списке разрешённых, раздала бы роль любому
// сертификату, подписанному нашим CA.
func peerIdentities(ctx context.Context) []string {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil
	}

	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return nil
	}

	if len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.VerifiedChains[0]) == 0 {
		return nil
	}
	leaf := tlsInfo.State.VerifiedChains[0][0]

	ids := make([]string, 0, len(leaf.DNSNames)+len(leaf.URIs))
	for _, name := range leaf.DNSNames {
		if name != "" {
			ids = append(ids, name)
		}
	}
	for _, uri := range leaf.URIs {
		if uri == nil {
			continue
		}
		if s := uri.String(); s != "" {
			ids = append(ids, s)
		}
	}

	return ids
}
