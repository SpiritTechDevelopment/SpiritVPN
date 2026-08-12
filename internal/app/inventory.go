package app

import (
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/nodeagent"
)

// Сверка desired state с фактическим инвентарём Xray (§16).
//
// Живёт в app, а не в domain, хотя правило чистое: ключ сравнения — открытый
// client_uuid, то есть секретный тип из crypto, а domain не зависит ни от crypto,
// ни от nodeagent и не должен начинать. Функция от этого чище не становится, но
// границы пакетов остаются прежними.

// DriftKind — вид расхождения. Он же метка метрики, поэтому переименование
// любого значения меняет контракт наблюдаемости §15.
type DriftKind string

const (
	// DriftMissing — desired present, а на ноде такого юзера нет.
	DriftMissing DriftKind = "missing"
	// DriftExtra — на ноде есть backend-owned юзер, которого desired не знает.
	DriftExtra DriftKind = "extra"
	// DriftMismatch — тот же accounting_id, но другой credential, flow или egress.
	DriftMismatch DriftKind = "mismatch"
)

// Причины, по которым наблюдение не годится для сверки (§16).
const (
	InventoryIncomplete   = "INVENTORY_INCOMPLETE"
	InventoryNotObserved  = "INVENTORY_NOT_OBSERVED"
	InventoryStale        = "INVENTORY_STALE"
	InventoryClockSkewed  = "INVENTORY_CLOCK_SKEWED"
	InventoryObservedFine = "INVENTORY_OBSERVED"
)

// InventoryVerdict — вывод сверки.
type InventoryVerdict struct {
	// Usable=false означает, что сверка не состоялась: наблюдение непригодно, и
	// «расхождений не найдено» из этого не следует.
	Usable bool
	// Reason — почему наблюдение отвергнуто либо принято. Стабилен: метка метрики.
	Reason string
	// Drift — сколько расхождений каждого вида. Пустая карта при Usable означает,
	// что нода совпадает с desired state.
	Drift map[DriftKind]int
}

// Drifted сообщает, что нода разошлась с desired state.
func (v InventoryVerdict) Drifted() bool { return len(v.Drift) > 0 }

// CompareInventory сверяет desired-набор с фактическим инвентарём ноды.
//
// Чистая: часы и порог передаются аргументами, в БД и в сеть не ходит.
//
// Непригодное наблюдение отвергается ЦЕЛИКОМ, а не частично (решение 88). §16
// запрещает выводить удаления из усечённого инвентаря, а чиним мы полным набором
// (решение 86), который удаляет по определению. Значит любой триггер по
// непригодному наблюдению был бы выводом удаления в обход запрета, и половинчатых
// вариантов здесь не существует.
func CompareInventory(
	desired []nodeagent.User,
	inventory nodeagent.Inventory,
	now time.Time,
	maxAge time.Duration,
) InventoryVerdict {
	if !inventory.Complete {
		return InventoryVerdict{Reason: InventoryIncomplete}
	}
	if inventory.ObservedAt.IsZero() {
		return InventoryVerdict{Reason: InventoryNotObserved}
	}

	age := now.Sub(inventory.ObservedAt)
	if age > maxAge {
		return InventoryVerdict{Reason: InventoryStale}
	}
	// Наблюдение из будущего означает разъехавшиеся часы ноды, а не свежесть.
	// Без этой проверки спешащий агент считался бы вечно свежим, и порог
	// устаревания не значил бы ничего именно там, где он нужен.
	if -age > maxAge {
		return InventoryVerdict{Reason: InventoryClockSkewed}
	}

	verdict := InventoryVerdict{Usable: true, Reason: InventoryObservedFine}

	// Чужой namespace не наш: агент не удаляет таких юзеров даже при complete-
	// наборе, поэтому расхождением они быть не могут — только вечным шумом.
	actual := make(map[string]nodeagent.User, len(inventory.Users))
	for _, user := range inventory.Users {
		if !user.BackendManaged {
			continue
		}
		actual[user.AccountingID] = user.User
	}

	for _, want := range desired {
		got, present := actual[want.AccountingID]
		switch {
		case !present:
			verdict.count(DriftMissing)
		case !sameUser(want, got):
			verdict.count(DriftMismatch)
		}
		delete(actual, want.AccountingID)
	}

	// Всё, что осталось, — backend-owned юзеры, которых desired state не знает.
	for range actual {
		verdict.count(DriftExtra)
	}

	return verdict
}

// sameUser сравнивает то, что определяет работоспособность доступа.
//
// egress_key сверх дословных «UUID/flow» §16 (решение 87): у BRIDGE он задаёт
// выходную ноду, и разошедшийся egress молча уводит трафик через чужой выход —
// доступ при этом работает, поэтому иначе об этом не узнает никто.
func sameUser(want, got nodeagent.User) bool {
	return want.ClientUUID.Reveal() == got.ClientUUID.Reveal() &&
		want.Flow == got.Flow &&
		want.EgressKey == got.EgressKey
}

func (v *InventoryVerdict) count(kind DriftKind) {
	if v.Drift == nil {
		v.Drift = make(map[DriftKind]int, 3)
	}
	v.Drift[kind]++
}
