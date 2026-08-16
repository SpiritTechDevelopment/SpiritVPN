package app_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	"github.com/RomanRyabinkin/SpiritVPN/internal/nodeagent"
)

// Сверка desired state с фактическим инвентарём Xray.

const inventoryMaxAge = 10 * time.Minute

// inventoryNow — «сейчас» сверки. Фиксированное: возраст наблюдения задаётся
// тестом, а не длительностью прогона.
var inventoryNow = time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)

var (
	uuidA = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	uuidB = uuid.MustParse("22222222-2222-4222-8222-222222222222")
)

func desiredUser(accountingID string, value uuid.UUID) nodeagent.User {
	return nodeagent.User{
		AccountingID: accountingID,
		ClientUUID:   crypto.NewClientUUID(value),
		Flow:         domain.FlowXTLSRprxVision,
		EgressKey:    "de-exit",
	}
}

// actualUser — то же самое, каким его увидели в Xray.
func actualUser(user nodeagent.User) nodeagent.ActualUser {
	return nodeagent.ActualUser{User: user, BackendManaged: true}
}

// freshInventory — пригодный снимок: полный и наблюдённый только что.
func freshInventory(users ...nodeagent.ActualUser) nodeagent.Inventory {
	return nodeagent.Inventory{
		Users:      users,
		ObservedAt: inventoryNow.Add(-time.Minute),
		Complete:   true,
	}
}

func TestCompareInventoryFindsDrift(t *testing.T) {
	present := desiredUser("u.aaa", uuidA)
	second := desiredUser("u.bbb", uuidB)

	rotated := desiredUser("u.aaa", uuidB)
	wrongFlow := present
	wrongFlow.Flow = "none"
	wrongEgress := present
	wrongEgress.EgressKey = "чужой-выход"

	for _, tc := range []struct {
		name    string
		desired []nodeagent.User
		actual  []nodeagent.ActualUser
		want    map[app.DriftKind]int
	}{
		{
			name:    "нода совпадает с desired state",
			desired: []nodeagent.User{present, second},
			actual:  []nodeagent.ActualUser{actualUser(present), actualUser(second)},
		},
		{
			name:    "юзера нет на ноде",
			desired: []nodeagent.User{present, second},
			actual:  []nodeagent.ActualUser{actualUser(present)},
			want:    map[app.DriftKind]int{app.DriftMissing: 1},
		},
		{
			name:    "на ноде лишний backend-owned юзер",
			desired: []nodeagent.User{present},
			actual:  []nodeagent.ActualUser{actualUser(present), actualUser(second)},
			want:    map[app.DriftKind]int{app.DriftExtra: 1},
		},
		{
			// Ротация credential: доступ формально есть, но работает по старому
			// uuid, и клиент с новой ссылкой не подключится.
			name:    "тот же accounting_id с другим credential",
			desired: []nodeagent.User{present},
			actual:  []nodeagent.ActualUser{actualUser(rotated)},
			want:    map[app.DriftKind]int{app.DriftMismatch: 1},
		},
		{
			name:    "тот же accounting_id с другим flow",
			desired: []nodeagent.User{present},
			actual:  []nodeagent.ActualUser{actualUser(wrongFlow)},
			want:    map[app.DriftKind]int{app.DriftMismatch: 1},
		},
		{
			// Сверх пары UUID и flow: доступ работает, но
			// трафик BRIDGE уходит через чужой выход.
			name:    "тот же accounting_id с другим egress",
			desired: []nodeagent.User{present},
			actual:  []nodeagent.ActualUser{actualUser(wrongEgress)},
			want:    map[app.DriftKind]int{app.DriftMismatch: 1},
		},
		{
			// Агент не удаляет чужих даже при complete-наборе, поэтому считать их
			// расхождением потребовало бы починки, которая не наступит.
			name:    "чужой namespace не наше дело",
			desired: []nodeagent.User{present},
			actual: []nodeagent.ActualUser{
				actualUser(present),
				{User: desiredUser("infra.probe", uuidB)},
			},
		},
		{
			name:    "пустая нода при пустом desired — это совпадение",
			desired: nil,
			actual:  nil,
		},
		{
			name:    "нода пуста, а доступы должны быть",
			desired: []nodeagent.User{present, second},
			actual:  nil,
			want:    map[app.DriftKind]int{app.DriftMissing: 2},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict := app.CompareInventory(
				tc.desired, freshInventory(tc.actual...), inventoryNow, inventoryMaxAge)

			if !verdict.Usable {
				t.Fatalf("снимок признан непригодным: %s", verdict.Reason)
			}
			if got, want := len(verdict.Drift), len(tc.want); got != want {
				t.Fatalf("видов расхождения %d, ожидалось %d: %v", got, want, verdict.Drift)
			}
			for kind, want := range tc.want {
				if got := verdict.Drift[kind]; got != want {
					t.Errorf("расхождений %s: %d, ожидалось %d", kind, got, want)
				}
			}
			if got := verdict.Drifted(); got != (len(tc.want) > 0) {
				t.Errorf("Drifted()=%v при расхождениях %v", got, verdict.Drift)
			}
		})
	}
}

// TestCompareInventoryRejectsUnusableSnapshot — непригодный
// снимок отвергается целиком.
//
// Usable=false здесь принципиально не то же самое, что «расхождений нет».
// Путаница между ними приняла бы «сравнить не с чем» за «нода в порядке» — а
// чинится нода как раз по результату сверки.
func TestCompareInventoryRejectsUnusableSnapshot(t *testing.T) {
	// Заведомо разошедшийся набор: если бы отбраковка не сработала, сверка нашла
	// бы расхождение, и тест не смог бы отличить одно от другого.
	desired := []nodeagent.User{desiredUser("u.aaa", uuidA)}

	for _, tc := range []struct {
		name      string
		inventory nodeagent.Inventory
		want      string
	}{
		{
			name:      "снимок усечён",
			inventory: nodeagent.Inventory{ObservedAt: inventoryNow, Complete: false},
			want:      app.InventoryIncomplete,
		},
		{
			name:      "полного наблюдения ещё не было",
			inventory: nodeagent.Inventory{Complete: true},
			want:      app.InventoryNotObserved,
		},
		{
			name: "наблюдение протухло",
			inventory: nodeagent.Inventory{
				ObservedAt: inventoryNow.Add(-inventoryMaxAge - time.Second),
				Complete:   true,
			},
			want: app.InventoryStale,
		},
		{
			// Часы ноды ушли вперёд. Без этой ветки спешащий агент считался бы
			// вечно свежим, и порог не значил бы ничего именно там, где нужен.
			name: "наблюдение из будущего",
			inventory: nodeagent.Inventory{
				ObservedAt: inventoryNow.Add(inventoryMaxAge + time.Second),
				Complete:   true,
			},
			want: app.InventoryClockSkewed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict := app.CompareInventory(desired, tc.inventory, inventoryNow, inventoryMaxAge)

			if verdict.Usable {
				t.Fatal("снимок принят к сверке")
			}
			if verdict.Reason != tc.want {
				t.Errorf("причина %q, ожидалась %q", verdict.Reason, tc.want)
			}
			if verdict.Drifted() {
				t.Errorf("по непригодному снимку выведено расхождение %v", verdict.Drift)
			}
		})
	}
}

// TestCompareInventoryAcceptsSnapshotAtAgeLimit — граница включительная: снимок
// ровно предельного возраста ещё годится. Без этого утверждения порог мог бы
// уехать на итерацию в любую сторону незамеченным.
func TestCompareInventoryAcceptsSnapshotAtAgeLimit(t *testing.T) {
	inventory := nodeagent.Inventory{
		ObservedAt: inventoryNow.Add(-inventoryMaxAge),
		Complete:   true,
	}

	verdict := app.CompareInventory(nil, inventory, inventoryNow, inventoryMaxAge)
	if !verdict.Usable {
		t.Fatalf("снимок предельного возраста отвергнут: %s", verdict.Reason)
	}
}
