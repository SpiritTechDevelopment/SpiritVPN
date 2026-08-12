package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/crypto"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	"github.com/RomanRyabinkin/SpiritVPN/internal/nodeagent"
)

// Сквозная проверка §18: открытый credential не появляется в логах ни на одном
// шаге жизненного цикла.
//
// Точечные поверхности проверены там, где живут: редакция ClientUUID — в crypto,
// отбрасывание тела запроса и ответа — в grpcsvc (VLESS URI возвращает именно
// ответ), отбрасывание SQL-аргументов — в cmd/spiritvpnd. Здесь не они, а то, что
// точечный тест поймать не может: любая новая запись в логе вдоль реального пути.

// reconcileSpy запоминает набор, ушедший на провод, и сообщает о найденном дрейфе.
//
// Набор нужен как позитивный контроль: агенту достаётся ОТКРЫТЫЙ client_uuid, и
// сверка его с uuid из VLESS URI доказывает, что искомая строка действительно
// проходила через код. Без этого тест искал бы в логах строку, которой в прогоне
// могло и не быть, и проходил бы по любой причине.
type reconcileSpy struct {
	users  []nodeagent.User
	result nodeagent.ReconcileResult
}

func (s *reconcileSpy) ReconcileUsers(
	_ context.Context,
	_ nodeagent.Endpoint,
	_ string,
	users []nodeagent.User,
) nodeagent.ReconcileResult {
	s.users = append(s.users, users...)
	return s.result
}

// TestIntegrationNoSecretsInLogs гоняет весь жизненный цикл и проверяет, что в
// логах нет ни открытого client_uuid, ни готовой ссылки.
func TestIntegrationNoSecretsInLogs(t *testing.T) {
	stack := newUsageStack(t)
	accountingID := seedUsageCustomer(t, stack, 1000)

	spy := &reconcileSpy{result: nodeagent.ReconcileResult{
		Outcome: nodeagent.Outcome{Result: domain.AttemptSucceeded, Code: nodeagent.CodeApplied},
		// Ненулевой дрейф поднимает уровень записи с debug до info: тест обязан
		// смотреть на ту ветку логирования, которая работает в бою.
		Removed:   2,
		Unchanged: 1,
	}}
	reconcile := app.NewReconcileNodes(
		New(stack.pool), spy, testCipher(t), crypto.NewGenerator(), testLogger(stack.logs),
		testReconcileOwner, testReconcileTTL, testReconcileInterval)

	// Успешный учёт: дельты, начисление, пересечение квоты и гашение доступа.
	now := time.Now().UTC()
	stack.agent.set("NL-1",
		batchOf(1, now, nodeagent.UserUsage{AccountingID: accountingID, UplinkBytes: 600, DownlinkBytes: 400}))
	pullRound(t, stack.usage)
	drainDispatch(t, stack.dispatch)

	// Потеря спула и недоступность агента: обе ветки логируются, вторая — ещё и
	// сообщением, пришедшим снаружи.
	stack.agent.set("NL-1", nodeagent.UsageBatch{
		Cursor:      nodeagent.UsageCursor{SpoolID: "spool-2", Sequence: 1},
		CollectedAt: now,
		Items:       []nodeagent.UserUsage{{AccountingID: accountingID, UplinkBytes: 1}},
	})
	pullRound(t, stack.usage)

	stack.agent.failWith = nodeagent.CodeUnavailable
	pullRound(t, stack.usage)
	stack.agent.failWith = ""

	// Полный набор ноды: единственное место, где открытый credential собирается
	// пачкой, — и потому самое опасное для логов.
	if progressed, err := reconcile.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ReconcileNodes.ProcessNext: %v", err)
	} else if !progressed {
		t.Fatal("reconcile не взял ни одной ноды: набор на провод не уходил")
	}

	links, err := stack.links.Execute(context.Background(), testCustomerID)
	if err != nil {
		t.Fatalf("GetCustomerAccessLinks: %v", err)
	}

	logs := stack.logs.String()
	if logs == "" {
		t.Fatal("за прогон не записано ни строчки лога — искать секреты негде")
	}

	// Позитивный контроль: uuid берётся из того, что реально ушло на провод.
	if len(spy.users) == 0 {
		t.Fatal("reconcile отправил пустой набор")
	}
	for _, user := range spy.users {
		secret := user.ClientUUID.Reveal().String()
		if secret == "" || strings.Count(secret, "-") != 4 {
			t.Fatalf("client_uuid %q не похож на UUID: проверка искала бы мусор", secret)
		}
		if strings.Contains(logs, secret) {
			t.Errorf("открытый client_uuid юзера %s попал в логи", user.AccountingID)
		}
	}

	// Готовая ссылка не логируется целиком: в ней тот же credential плюс
	// параметры подключения (§8).
	var ready int
	for _, link := range links {
		if link.URI == "" {
			continue
		}
		ready++
		if strings.Contains(logs, link.URI) {
			t.Errorf("VLESS URI доступа %s попала в логи", link.Kind)
		}
	}
	if ready == 0 {
		t.Fatal("ни одной READY-ссылки: проверять на утечку нечего")
	}
	if strings.Contains(logs, "vless://") {
		t.Error("в логах есть ссылка, не совпавшая ни с одной выданной")
	}
}
