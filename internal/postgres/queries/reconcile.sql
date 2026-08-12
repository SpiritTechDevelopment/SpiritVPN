-- Authoritative reconcile: приведение ноды к точному desired state (§10).
--
-- Обычные изменения доставляет диспетчер по одному юзеру (§9); эти запросы — про
-- инициализацию и починку, то есть про случаи, когда доставлять уже нечего,
-- потому что нода потеряла состояние целиком или разошлась с ним незаметно.

-- Берёт ноду, которую пора reconcile-ить.
--
-- Два повода, оба из §10. needs_bootstrap — локальное состояние агента новое или
-- повреждено, и без полного набора он не имеет права удалять юзеров, то есть
-- сам из этого состояния не выйдет. Таймер — периодическая коррекция дрейфа:
-- расхождение, о котором никто не сообщил, иначе жило бы вечно.
--
-- Рост desired_revision поводом НЕ является: обычные изменения доставляет
-- диспетчер, и полный набор на каждую команду customer был бы дороже её самой.
-- Вендорный контракт говорит то же прямым текстом: «Normal changes use the two
-- Ensure methods; this method is for initialization and recovery».
--
-- reconcile_attempted_at ставится при захвате, а не по завершении: нода, у
-- которой reconcile стабильно не удаётся, иначе опрашивалась бы циклом без пауз.
-- name: ClaimNodeForReconcile :one
WITH claimed AS (
    UPDATE vpn_nodes
    SET reconcile_lease_owner      = @owner::text,
        reconcile_lease_expires_at = now() + make_interval(secs => @lease_seconds::int),
        reconcile_attempted_at     = now()
    WHERE node_id = (
        SELECT n.node_id
        FROM vpn_nodes n
        WHERE n.current
          AND (n.reconcile_lease_expires_at IS NULL OR n.reconcile_lease_expires_at < now())
          AND (
              n.needs_bootstrap
              OR n.reconcile_attempted_at IS NULL
              OR n.reconcile_attempted_at < now() - make_interval(secs => @min_interval_seconds::int)
          )
        -- NULLS FIRST: нода, которую не reconcile-или ни разу, идёт вперёд всех.
        ORDER BY n.reconcile_attempted_at NULLS FIRST
        FOR UPDATE SKIP LOCKED
        LIMIT 1
    )
    RETURNING node_id, agent_config, public_config, desired_revision
)
SELECT node_id, agent_config, public_config, desired_revision FROM claimed;

-- Полный набор desired PRESENT юзеров ноды (§10).
--
-- «Фактически разрешённые» означает три ограничения, а не одно. Истёкшие
-- entitlement и исчерпавшие node quota в набор не входят, ДАЖЕ если expiry или
-- usage worker ещё не успели перевести их access в ABSENT: §10 требует этого
-- явно, а reconcile авторитетен — то, чего нет в наборе, агент удалит.
--
-- Порядок по access_id, чтобы payload двух подряд идущих reconcile отличался
-- только содержимым, а не перестановкой.
-- name: ListNodeDesiredUsers :many
SELECT a.access_id,
       a.accounting_id,
       a.egress_key,
       a.encrypted_client_uuid,
       a.encryption_key_id
FROM vpn_accesses a
JOIN customer_entitlements e ON e.customer_id = a.customer_id
WHERE a.entry_node_id = @node_id::text
  AND a.retired_at IS NULL
  AND a.desired_state = 'PRESENT'
  AND e.expires_at > now()
  AND NOT EXISTS (
      SELECT 1
      FROM quota_periods p
      JOIN node_quota_usage u ON u.quota_period_id = p.quota_period_id
      WHERE p.customer_id = a.customer_id
        AND p.closed_at IS NULL
        AND u.node_id = a.entry_node_id
        AND u.exhausted_at IS NOT NULL
  )
ORDER BY a.access_id;

-- Снимает собственный lease reconcile.
--
-- Собственный: чужой мог быть взят после того, как наш протух, и снимать его
-- значило бы пустить третьего к той же ноде.
-- name: ReleaseNodeReconcile :exec
UPDATE vpn_nodes
SET reconcile_lease_owner      = NULL,
    reconcile_lease_expires_at = NULL
WHERE node_id = @node_id::text
  AND reconcile_lease_owner = @owner::text;

-- Принимает результат reconcile, если desired state ноды не уехал (§10).
--
-- Условие в WHERE, а не отдельным чтением, и по той же причине, что у
-- SetAccessApplyState: только так проверка атомарна. Ноль строк означает, что
-- desired_revision сдвинулась, пока мы ходили к агенту, — набор на проводе уже
-- устарел, принимать его нельзя.
--
-- Ревизия сравнивается ЗДЕСЬ, а не приезжает от агента: вендорный контракт
-- ReconcileUsers её не передаёт и не возвращает (решение 82).
-- name: AcceptNodeReconcile :execrows
UPDATE vpn_nodes
SET reconciled_revision = @desired_revision,
    reconciled_at       = now()
WHERE node_id = @node_id::text
  AND desired_revision = @desired_revision;

-- Отмечает применёнными те desired states ноды, которые reconcile удовлетворил.
--
-- Два множества, и их различие существенно. Отправленные access теперь есть в
-- Xray — это applied_ids. ABSENT-access теперь в Xray отсутствуют, потому что
-- полный набор их не содержал, а всё лишнее агент удаляет, — это и есть
-- авторитетность.
--
-- А вот PRESENT-access, не попавший в набор из-за истёкшего entitlement или
-- исчерпанной квоты, здесь НЕ отмечается: он удалён с ноды, но его desired_state
-- всё ещё PRESENT, и APPLIED означал бы «на ноде ровно то, что заказано», что
-- неправда. Его приведёт в порядок expiry или usage worker, сменив desired_state.
-- name: MarkNodeAccessesApplied :exec
UPDATE vpn_accesses
SET apply_state = 'APPLIED'
WHERE entry_node_id = @node_id::text
  AND retired_at IS NULL
  AND (access_id = ANY(@applied_ids::uuid[]) OR desired_state = 'ABSENT')
  AND apply_state <> 'APPLIED';

-- Завершает операции ноды, которые reconcile уже удовлетворил (§10).
--
-- Только ожидающие: IN_FLIGHT держит другая горутина диспетчера, и трогать её
-- строку значило бы дописывать результат за неё. Она завершится сама и увидит
-- собственную повторную проверку desired_version.
--
-- Условие безопасно ровно потому, что вызывающий уже прошёл гейт по
-- desired_revision: раз ревизия ноды не менялась с момента чтения набора, любое
-- изменение desired state любого её access тоже не происходило — оно обязано
-- двигать ревизию. Значит каждая ожидающая операция несёт состояние, которое
-- полный набор только что доставил.
-- name: CompleteNodeOperationsByReconcile :exec
UPDATE agent_operations
SET status          = 'SUCCEEDED',
    completed_at    = now(),
    next_attempt_at = NULL,
    last_error_code = @error_code::text
WHERE node_id = @node_id::text
  AND status IN ('PENDING', 'RETRY_WAIT');

-- Запоминает признак bootstrap, полученный от агента (§10).
--
-- Пишет pull worker: он и так разговаривает с каждой нодой каждые 15 секунд, а
-- reconcile-воркер спит между заходами и узнал бы об этом с задержкой в целый
-- интервал.
--
-- Условие на неравенство существенно: без него каждый опрос каждой ноды писал бы
-- новую версию строки vpn_nodes независимо от того, изменилось ли хоть что-то.
-- name: SetNodeNeedsBootstrap :exec
UPDATE vpn_nodes
SET needs_bootstrap = @needs_bootstrap::boolean
WHERE node_id = @node_id::text
  AND needs_bootstrap <> @needs_bootstrap::boolean;
