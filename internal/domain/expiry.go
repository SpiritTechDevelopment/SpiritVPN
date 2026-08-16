package domain

import "time"

// ExpiryPlan — что снять с истёкшего customer.
//
// Список полей заметно короче, чем у ApplyPlan и MaterializationPlan, и это не
// упущение: истечение ничего не создаёт и ничего не ретайрит, оно только гасит
// уже существующее.
type ExpiryPlan struct {
	// DesiredChanges — access, уходящие в ABSENT. Отсортированы по access_id
	// (нормативный порядок блокировок).
	DesiredChanges []DesiredChange

	// TouchedNodes — ноды, с которых снимаются юзеры. Их строки блокируются в этом
	// порядке, и каждой ровно один раз увеличивается desired_revision, сколько бы
	// access customer на ней ни было.
	TouchedNodes []NodeID

	// EntitlementDesiredVersion — новое значение счётчика корневой строки; на
	// пустом плане равно прежнему.
	EntitlementDesiredVersion int64
}

// IsNoOp сообщает, что снимать нечего: у customer уже не осталось PRESENT access.
func (p ExpiryPlan) IsNoOp() bool {
	return len(p.DesiredChanges) == 0
}

// PlanExpiry строит план снятия доступа у истёкшего customer.
//
// Причина TIME_EXPIRED выводится прямо из expires_at и нигде не хранится: ни
// отдельной block row, ни отдельного effective state под неё нет. Поэтому
// функция не принимает признак истечения параметром, а решает сама
// по тем же часам, что и остальной домен.
//
// Квота сюда не передаётся намеренно. Для истёкшего customer DesiredStateFor
// вернёт ABSENT при любом её состоянии, поэтому читать node_quota_usage и брать
// его locks воркеру не нужно — шаг 3 нормативного порядка просто пропускается.
//
// accesses — все нератайрнутые access customer, а не только согласованные с
// топологией, как в PlanApply. Рассогласованный access истёкшего customer тоже
// обязан погаснуть: материализация позже его ретайрит, а ретайр — это тот же
// ABSENT, поэтому конфликта состояний не возникает.
func PlanExpiry(now time.Time, entitlement Entitlement, accesses []Access) ExpiryPlan {
	plan := ExpiryPlan{EntitlementDesiredVersion: entitlement.DesiredVersion}

	// Неистёкший customer сюда попадает штатно: выборка воркера могла устареть,
	// пока он ждал lock корневой строки, а renewal мог уже закоммититься.
	// Пустой план — правильный ответ, а не повод для ошибки.
	if now.Before(entitlement.ExpiresAt) {
		return plan
	}

	// nil вместо карты исчерпания: у истёкшего customer её значение ни на что не
	// влияет, и подставлять сюда пустую карту было бы честнее только на вид.
	plan.DesiredChanges = PlanDesiredChanges(accesses, now, entitlement.ExpiresAt, nil)
	plan.TouchedNodes = touchedNodes(nil, plan.DesiredChanges)

	if !plan.IsNoOp() {
		plan.EntitlementDesiredVersion++
	}
	return plan
}
