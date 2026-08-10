package domain

import "time"

// LinkState — состояние ссылки в ответе GetCustomerAccessLinks (§5).
type LinkState string

const (
	LinkStatePending LinkState = "PENDING"
	LinkStateReady   LinkState = "READY"
	LinkStateBlocked LinkState = "BLOCKED"
	LinkStateFailed  LinkState = "FAILED"
)

// BlockReason — причина блокировки. Обязательна при BLOCKED и отсутствует при
// остальных состояниях (§5), поэтому пустое значение — часть контракта, а не
// «неизвестно».
type BlockReason string

const (
	BlockReasonNone                  BlockReason = ""
	BlockReasonTimeExpired           BlockReason = "TIME_EXPIRED"
	BlockReasonTrafficQuotaExhausted BlockReason = "TRAFFIC_QUOTA_EXHAUSTED"
)

// LinkStatus — состояние одной ссылки вместе с причиной блокировки.
type LinkStatus struct {
	State  LinkState
	Reason BlockReason
}

// LinkInput — всё, из чего выводится состояние одной ссылки.
//
// Отдельного effective/block state в схеме нет: TIME_EXPIRED выводится из
// expires_at корневой строки, TRAFFIC_QUOTA_EXHAUSTED — из непустого
// node_quota_usage.exhausted_at текущего периода (§5). Поэтому вход состоит из
// сырых фактов, а не из заранее посчитанного состояния.
type LinkInput struct {
	// Now — время PostgreSQL, а не часов процесса (решение 2): expires_at
	// сравнивается со временем той же базы, где он хранится.
	Now       time.Time
	ExpiresAt time.Time

	// QuotaExhausted — exhausted_at IS NOT NULL у входной ноды access в текущем
	// периоде. Квота применяется на каждой ноде независимо (§4).
	QuotaExhausted bool

	DesiredState DesiredState
	ApplyState   ApplyState

	// EntryUsable — параметры входной ноды пригодны для сборки URI (§8: всё,
	// кроме uuid, берётся из node.public входной ноды). Непригодные параметры
	// означают рассогласованную проекцию: §6 валидирует их до записи. Наружу это
	// уходит как FAILED одной ссылки, а не отказом всего ответа — недоступность
	// одной ноды не скрывает работающие ссылки других (§5, решение 18).
	EntryUsable bool
}

// LinkStatusOf выводит состояние одной ссылки (§5, §8).
//
// Функция тотальна по четырём состояниям контракта, и порядок ветвей в ней и
// есть нормативное правило приоритета:
//
//  1. истёкший срок действует на всего customer сразу (§5);
//  2. исчерпанная квота — только на его access на этой ноде (§4). Порядок 1 и 2
//     существенен: при одновременно применимых причинах наружу уходит
//     TIME_EXPIRED (§5). В WHERE это правило не выражается — оно не про то,
//     выдавать ли URI (её не будет в обоих случаях), а про то, какую причину
//     показать;
//  3. недоставленная навсегда операция и непригодная входная нода одинаково
//     означают «ссылка сама не появится» — в отличие от PENDING, который
//     обещает, что появится;
//  4. READY — ровно конъюнкция §8 за вычетом уже проверенного выше и за вычетом
//     присутствия цели в текущем manifest: отсутствующая цель до этой функции не
//     доходит, её отсекает запрос (решение 17);
//  5. остальное — PENDING: PENDING и RETRYING доставки, а также ABSENT без
//     действующей причины блокировки. Последнее — переходное состояние между
//     снятием блокировки и доставкой нового desired state.
func LinkStatusOf(in LinkInput) LinkStatus {
	switch {
	case !in.Now.Before(in.ExpiresAt):
		return LinkStatus{State: LinkStateBlocked, Reason: BlockReasonTimeExpired}

	case in.QuotaExhausted:
		return LinkStatus{State: LinkStateBlocked, Reason: BlockReasonTrafficQuotaExhausted}

	case in.ApplyState == ApplyStateFailed, !in.EntryUsable:
		return LinkStatus{State: LinkStateFailed, Reason: BlockReasonNone}

	case in.DesiredState == DesiredStatePresent && in.ApplyState == ApplyStateApplied:
		return LinkStatus{State: LinkStateReady, Reason: BlockReasonNone}

	default:
		return LinkStatus{State: LinkStatePending, Reason: BlockReasonNone}
	}
}
