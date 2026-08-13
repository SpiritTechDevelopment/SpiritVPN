package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	"github.com/RomanRyabinkin/SpiritVPN/internal/nodeagent"
)

// Коды исходов, которые производит сам reconcile, а не агент и не транспорт.
//
// Как и у диспетчера, стоят рядом с тем, кто их порождает, и как и там,
// переименование любого — изменение контракта наблюдаемости.
const (
	// CodeReconcileStale — desired state ноды уехал, пока набор был на проводе.
	CodeReconcileStale = "RECONCILE_STALE"
	// CodeReconcileApplied — исход, которым reconcile завершает удовлетворённые
	// операции ноды. Попадает в last_error_code, потому что отдельной колонки под
	// «чем именно закрыто» у операции нет.
	CodeReconcileApplied = "RECONCILE_APPLIED"
)

// ReconcileNodes — authoritative reconcile.
//
// Второй слой восстановления ноды и единственный источник удалений. Первый слой,
// add-only self-heal из локального снапшота, живёт на агенте, поднимает юзеров
// после рестарта Xray и не удаляет НИКОГДА: его снапшот может отстать, и
// удаление по отставшему снапшоту сняло бы живого юзера. Лишний юзер переживаем,
// потерянный — нет, поэтому все удаления здесь.
//
// Обычные изменения сюда не приходят: их по одному доставляет диспетчер.
// Reconcile нужен там, где доставлять уже нечего, — нода потеряла состояние
// целиком (needs_bootstrap) или разошлась с ним незаметно (таймер).
//
// Шаг состоит из двух транзакций с сетевым вызовом между ними: запрещено
// держать транзакцию открытой во время обращения к агенту.
//
//	tx1  lease + зафиксированная desired_revision + полный набор
//	—    расшифрование client_uuid и RPC
//	tx2  приём результата с повторной проверкой desired_revision
type ReconcileNodes struct {
	Repo   ReconcileRepository
	Agent  ReconcileAgent
	Sealer CredentialSealer
	IDs    IDs
	Clock  Clock
	Logger *slog.Logger

	// Owner попадает в reconcile_lease_owner: по нему видно, чей lease протух.
	Owner string
	// LeaseTTL должен с запасом перекрывать nodeagent.DefaultReconcileTimeout:
	// истёкший раньше ответа lease пустил бы второй полный набор на ту же ноду.
	LeaseTTL time.Duration
	// MinInterval — как часто нода reconcile-ится по таймеру. Он же потолок жизни
	// расхождения, о котором никто не сообщил.
	MinInterval time.Duration

	// MaxObservationAge — предел давности наблюдения Xray. Инвентарь старше него
	// описывает уже неизвестно что, и сверяться с ним нельзя.
	MaxObservationAge time.Duration
}

// NewReconcileNodes собирает воркер из портов.
func NewReconcileNodes(
	repo ReconcileRepository,
	agent ReconcileAgent,
	sealer CredentialSealer,
	ids IDs,
	clock Clock,
	logger *slog.Logger,
	owner string,
	leaseTTL, minInterval, maxObservationAge time.Duration,
) *ReconcileNodes {
	return &ReconcileNodes{
		Repo:              repo,
		Agent:             agent,
		Sealer:            sealer,
		IDs:               ids,
		Clock:             clock,
		Logger:            logger,
		Owner:             owner,
		LeaseTTL:          leaseTTL,
		MinInterval:       minInterval,
		MaxObservationAge: maxObservationAge,
	}
}

// ProcessNext приводит к desired state ОДНУ ноду.
//
// progressed=false означает, что reconcile-ить сейчас нечего. Недоступная нода
// прогрессом считается: работа проделана, и повторять её немедленно не надо —
// темп задаёт MinInterval, а не цикл воркера.
func (uc *ReconcileNodes) ProcessNext(ctx context.Context) (progressed bool, err error) {
	node, err := uc.Repo.ClaimNodeForReconcile(ctx, uc.Owner, uc.LeaseTTL, uc.MinInterval)
	if err != nil {
		return false, fmt.Errorf("взятие ноды на reconcile: %w", err)
	}
	if node == nil {
		return false, nil
	}

	// Lease снимается на любом выходе, включая отказ: иначе нода простаивала бы до
	// истечения TTL, который заметно длиннее интервала.
	defer uc.release(ctx, node.NodeID)

	users, ok := uc.materialize(ctx, *node)
	if !ok {
		return true, nil
	}

	// Полный набор — дорогой вызов: до 2000 юзеров, каждого агент применяет к Xray
	// по одному. Ноде с потерянным состоянием он нужен целиком, всем остальным
	// достаточно убедиться, что расхождения нет. До этого среза набор летел
	// на каждую ноду каждый интервал, и дрейф чинился вслепую.
	if !node.NeedsBootstrap && !uc.drifted(ctx, *node, users) {
		return true, nil
	}

	operationID, err := uc.IDs.NewOperationID()
	if err != nil {
		return false, fmt.Errorf("идентификатор reconcile: %w", err)
	}

	result := uc.Agent.ReconcileUsers(ctx, node.Endpoint, operationID.String(), users)
	if result.Result != domain.AttemptSucceeded {
		// Недоступность ноды — не отказ шага: она не меняет ни
		// состава fleet, ни desired state. Повтор произойдёт сам собой, потому что
		// повод для reconcile никуда не делся.
		uc.logFailure(ctx, node.NodeID, result)
		return true, nil
	}

	return true, uc.accept(ctx, *node, result)
}

// drifted сверяет ноду с её фактическим инвентарём.
//
// false означает «полный набор сейчас не нужен», а не «нода в порядке»: сюда же
// попадают недоступный агент и непригодное наблюдение. Это осознанно. Гнать набор
// на ноду, которая только что не ответила, бессмысленно, а гнать его по
// непригодному наблюдению запрещено. В обоих случаях ничего не
// потеряно: повод для reconcile никуда не делся, и следующий интервал повторит
// попытку.
func (uc *ReconcileNodes) drifted(ctx context.Context, node ClaimedReconcileNode, users []nodeagent.User) bool {
	outcome := uc.Agent.ObserveUsers(ctx, node.Endpoint)
	if !outcome.OK() {
		uc.Logger.LogAttrs(ctx, slog.LevelWarn, "инвентарь ноды не получен",
			slog.String("node_id", string(node.NodeID)),
			slog.String("code", outcome.Code),
			slog.String("message", outcome.Message))
		return false
	}

	verdict := CompareInventory(users, *outcome.Inventory, uc.Clock.Now(), uc.MaxObservationAge)
	if !verdict.Usable {
		// Уровень info, а не warn: усечённый или ещё не сделанный снимок — штатное
		// состояние агента, который только что поднялся. Тревожит не сам факт, а
		// его повторяемость, и это видно по метрике, а не по одной записи.
		uc.Logger.LogAttrs(ctx, slog.LevelInfo, "инвентарь ноды непригоден для сверки",
			slog.String("node_id", string(node.NodeID)),
			slog.String("reason", verdict.Reason))
		return false
	}

	if !verdict.Drifted() {
		return false
	}

	uc.Logger.LogAttrs(ctx, slog.LevelWarn, "нода разошлась с desired state",
		slog.String("node_id", string(node.NodeID)),
		slog.Int("missing", verdict.Drift[DriftMissing]),
		slog.Int("extra", verdict.Drift[DriftExtra]),
		slog.Int("mismatch", verdict.Drift[DriftMismatch]))
	return true
}

// materialize расшифровывает набор. ok=false означает, что отправлять нельзя.
//
// Единственное место среза, где ошибка обязана останавливать ВЕСЬ шаг, а не
// пропускать проблемную запись. Набор авторитетен: юзер, выпавший из него,
// будет с ноды удалён. Пропустить нерасшифровавшегося значило бы своими руками
// снять рабочий доступ — то есть превратить отказ ключа в потерю сервиса.
func (uc *ReconcileNodes) materialize(ctx context.Context, node ClaimedReconcileNode) ([]nodeagent.User, bool) {
	// Пустой flow означает, что public_config ноды не разобрался: колонка битая
	// либо не заполнена. Отправить набор с чужим flow — сломать на ноде всех
	// разом, поэтому такая нода пропускается целиком (ссылки гаснут по этой же
	// причине одну ссылку, здесь на кону вся нода).
	if node.Flow == "" {
		uc.Logger.LogAttrs(ctx, slog.LevelError, "reconcile пропущен: public_config ноды непригоден",
			slog.String("node_id", string(node.NodeID)))
		return nil, false
	}

	users := make([]nodeagent.User, 0, len(node.Users))
	for _, user := range node.Users {
		// Открытый client_uuid живёт только внутри этого вызова: payload
		// собирается в память и не логируется.
		clientUUID, err := uc.Sealer.Open(user.Credential)
		if err != nil {
			uc.Logger.LogAttrs(ctx, slog.LevelError, "reconcile пропущен: client_uuid не расшифровывается",
				slog.String("node_id", string(node.NodeID)),
				slog.String("accounting_id", user.AccountingID),
				slog.Any("error", err))
			return nil, false
		}

		users = append(users, nodeagent.User{
			AccountingID: user.AccountingID,
			ClientUUID:   clientUUID,
			Flow:         node.Flow,
			EgressKey:    user.EgressKey,
		})
	}

	return users, true
}

// accept записывает результат отдельной транзакцией с повторной проверкой
// desired_revision.
func (uc *ReconcileNodes) accept(
	ctx context.Context,
	node ClaimedReconcileNode,
	result nodeagent.ReconcileResult,
) error {
	applied := make([]uuid.UUID, 0, len(node.Users))
	for _, user := range node.Users {
		applied = append(applied, user.AccessID)
	}

	accepted, err := uc.Repo.AcceptReconcile(ctx, ReconcileAcceptance{
		NodeID:           node.NodeID,
		DesiredRevision:  node.DesiredRevision,
		AppliedAccessIDs: applied,
	})
	if err != nil {
		return fmt.Errorf("приём результата reconcile: %w", err)
	}

	if !accepted {
		// desired state уехал, пока набор был на проводе. Ничего не потеряно:
		// более новое состояние уже доставляют обычные Ensure, а следующий проход
		// сверит набор заново.
		uc.Logger.LogAttrs(ctx, slog.LevelInfo, "результат reconcile устарел",
			slog.String("node_id", string(node.NodeID)),
			slog.String("code", CodeReconcileStale),
			slog.Int64("desired_revision", node.DesiredRevision))
		return nil
	}

	uc.logAccepted(ctx, node, result)
	return nil
}

// logAccepted сообщает о принятом наборе.
//
// Уровень зависит от того, нашлось ли расхождение: removed на живой ноде и есть
// тот дрейф, ради которого reconcile существует, и чинить его молча значило бы
// не узнать, что он вообще бывает.
func (uc *ReconcileNodes) logAccepted(
	ctx context.Context,
	node ClaimedReconcileNode,
	result nodeagent.ReconcileResult,
) {
	level := slog.LevelDebug
	if result.Added+result.Replaced+result.Removed > 0 {
		level = slog.LevelInfo
	}

	uc.Logger.LogAttrs(ctx, level, "нода приведена к desired state",
		slog.String("node_id", string(node.NodeID)),
		slog.Int64("desired_revision", node.DesiredRevision),
		slog.Int("users", len(node.Users)),
		slog.Uint64("added", uint64(result.Added)),
		slog.Uint64("replaced", uint64(result.Replaced)),
		slog.Uint64("removed", uint64(result.Removed)),
		slog.Uint64("unchanged", uint64(result.Unchanged)))
}

// release снимает lease, не заслоняя основную ошибку шага.
func (uc *ReconcileNodes) release(ctx context.Context, nodeID domain.NodeID) {
	// Контекст мог быть отменён вместе с процессом. Тогда снимать lease нечем и
	// незачем: он протухнет сам.
	if ctx.Err() != nil {
		return
	}
	if err := uc.Repo.ReleaseNodeReconcile(ctx, nodeID, uc.Owner); err != nil {
		uc.Logger.LogAttrs(ctx, slog.LevelWarn, "не удалось снять lease reconcile",
			slog.String("node_id", string(nodeID)),
			slog.Any("error", err))
	}
}

func (uc *ReconcileNodes) logFailure(ctx context.Context, nodeID domain.NodeID, result nodeagent.ReconcileResult) {
	level := slog.LevelWarn
	if result.Alert {
		level = slog.LevelError
	}

	uc.Logger.LogAttrs(ctx, level, "reconcile ноды не удался",
		slog.String("node_id", string(nodeID)),
		slog.String("code", result.Code),
		slog.String("message", result.Message))
}
