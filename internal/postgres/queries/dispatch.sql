-- Диспетчер agent-операций: чтение outbox и запись исходов доставки.
--
-- Транзакций здесь две, и между ними идёт сетевой вызов: нельзя держать
-- открытую транзакцию во время обращения к node-agent. Первая берёт lease и
-- собирает payload, вторая записывает результат.

-- Собирает операции, чей lease протух: воркер, взявший их, до записи результата не
-- дожил.
--
-- Развилка: устаревшая desired_version не повторяется
-- никогда и становится терминальной SUPERSEDED, актуальная возвращается в очередь.
-- next_attempt_at = now(), а не now() + backoff: lease протух из-за смерти воркера,
-- а не из-за отказа ноды, и сам TTL уже был паузой.
--
-- attempt_count не трогается: он был увеличен при взятии lease, и
-- попытка действительно состоялась — backend просто не узнал, чем она кончилась.
--
-- SKIP LOCKED и LIMIT, потому что сборщиков восемь: без них все восемь встали бы в
-- очередь на один и тот же набор строк.
-- name: ReapExpiredOperationLeases :execrows
UPDATE agent_operations o
SET status = CASE WHEN a.desired_version > o.desired_version
                  THEN 'SUPERSEDED' ELSE 'RETRY_WAIT' END,
    completed_at = CASE WHEN a.desired_version > o.desired_version
                        THEN now() ELSE NULL END,
    next_attempt_at = CASE WHEN a.desired_version > o.desired_version
                           THEN NULL ELSE now() END,
    lease_owner      = NULL,
    lease_expires_at = NULL
FROM vpn_accesses a
WHERE a.access_id = o.access_id
  AND o.operation_id IN (
      SELECT e.operation_id
      FROM agent_operations e
      WHERE e.status = 'IN_FLIGHT'
        AND e.lease_expires_at < now()
      ORDER BY e.lease_expires_at
      FOR UPDATE SKIP LOCKED
      LIMIT @max_reaped::int
  );

-- Берёт lease готовой к отправке операции и сразу отдаёт payload.
--
-- Гейт «на этой ноде нет IN_FLIGHT» отсекает конкурента
-- «одновременно на одну ноду отправляется не более одной mutating operation». Сам
-- по себе он неполон: два воркера читают один committed-снимок и оба его проходят.
-- Инвариант держит partial unique index agent_operations_single_in_flight_per_node,
-- а этот гейт лишь избавляет от лишних unique violation в обычном случае.
--
-- Протухшая IN_FLIGHT ноду тоже держит, и это намеренно: слот освобождает сборщик
-- выше, в той же транзакции. Иначе гейт расходился бы с индексом, и каждая попытка
-- на такой ноде стоила бы отката.
--
-- attempt_count увеличивается здесь, а не при записи результата: воркер, упавший
-- после RPC, иначе не оставил бы следа попытки и backoff не рос бы по кругу.
-- Первая неудача приходит в домен со счётчиком 1, что даёт заявленную
-- начальную задержку в секунду.
--
-- Payload собирается из актуальных строк access и ноды, а не из операции:
-- хранить его в outbox запрещено. encrypted_client_uuid читается только для
-- ENSURE_PRESENT — удаление матчится по accounting_id, и выносить ради него секрет
-- в память не нужно.
--
-- access_desired_version возвращается для проверки перед RPC: если версия уже ушла
-- вперёд, звонить агенту не нужно.
-- name: LeaseNextOperation :one
WITH leased AS (
    UPDATE agent_operations
    SET status           = 'IN_FLIGHT',
        attempt_count    = attempt_count + 1,
        lease_owner      = @owner::text,
        lease_expires_at = now() + make_interval(secs => @lease_seconds::int),
        next_attempt_at  = NULL
    WHERE operation_id = (
        SELECT o.operation_id
        FROM agent_operations o
        JOIN vpn_nodes gate ON gate.node_id = o.node_id
        WHERE o.status IN ('PENDING', 'RETRY_WAIT')
          AND o.next_attempt_at <= now()
          AND (gate.reconcile_lease_expires_at IS NULL
               OR gate.reconcile_lease_expires_at < now())
          AND NOT EXISTS (
              SELECT 1
              FROM agent_operations f
              WHERE f.node_id = o.node_id
                AND f.status = 'IN_FLIGHT'
          )
        ORDER BY o.next_attempt_at, o.operation_id
        -- Блокируется именно строка ноды, а не operation: общий порядок
        -- customer writers — vpn_nodes перед agent_operations. После возврата
        -- id внешний UPDATE заблокирует operation вторым. Вторая dispatch-
        -- горутина пропустит уже занятую ноду по SKIP LOCKED.
        FOR UPDATE OF gate SKIP LOCKED
        LIMIT 1
    )
      -- Subquery блокирует node gate, но намеренно не строку operation, чтобы
      -- соблюдать общий порядок vpn_nodes -> agent_operations. Поэтому между
      -- снимком subquery и блокировкой целевой строки другой writer может уже
      -- перевести выбранную operation в IN_FLIGHT/SUPERSEDED. UPDATE обязан
      -- повторно проверить eligibility после ожидания row lock, иначе он
      -- «переарендует» уже IN_FLIGHT строку и обойдёт unique index той же строкой.
      AND status IN ('PENDING', 'RETRY_WAIT')
      AND next_attempt_at <= now()
    RETURNING operation_id, node_id, access_id, operation_type, desired_version, attempt_count
)
SELECT l.operation_id,
       l.node_id,
       l.access_id,
       l.operation_type,
       l.desired_version,
       l.attempt_count,
       a.desired_version AS access_desired_version,
       a.accounting_id,
       a.egress_key,
       (CASE WHEN l.operation_type = 'ENSURE_PRESENT'
             THEN a.encrypted_client_uuid END)::bytea AS encrypted_client_uuid,
       (CASE WHEN l.operation_type = 'ENSURE_PRESENT'
             THEN a.encryption_key_id ELSE '' END)::text AS encryption_key_id,
       n.agent_config,
       n.public_config
FROM leased l
JOIN vpn_accesses a ON a.access_id = l.access_id
JOIN vpn_nodes n ON n.node_id = l.node_id;

-- Проецирует исход доставки на строку access с повторной проверкой desired_version.
--
-- Проверка живёт в WHERE, а не в отдельном чтении, потому что только так она
-- атомарна: под READ COMMITTED конкурирующий UPDATE этой строки заставит наш
-- дождаться его commit и перепроверить условие уже по новой версии строки. Ушедшая
-- вперёд desired_version даёт ноль затронутых строк — ровно то, что требуется
-- («результат устаревшей operation не меняет apply_state актуальной desired
-- version»).
--
-- Ноль строк — это ещё и ответ на вопрос «устарела ли операция», по которому
-- вызывающий решает судьбу самой операции. Отдельного SELECT для этого
-- не нужно.
-- name: SetAccessApplyState :execrows
UPDATE vpn_accesses
SET apply_state = @apply_state::text
WHERE access_id = @access_id
  AND desired_version = @desired_version;

-- Записывает терминальный либо ожидающий повтора статус операции.
--
-- Lease снимается всегда: операция либо завершена, либо ждёт next_attempt_at, и в
-- обоих случаях ноду держать больше не за что. Диагностика агента кладётся в
-- last_error_* как есть — она уже санитизирована на его стороне и секретов не
-- содержит.
-- name: CompleteOperation :exec
UPDATE agent_operations
SET status             = @status::text,
    next_attempt_at    = @next_attempt_at,
    completed_at       = CASE WHEN @completed::boolean THEN now() ELSE NULL END,
    last_error_code    = @error_code::text,
    last_error_message = @error_message::text,
    lease_owner        = NULL,
    lease_expires_at   = NULL
WHERE operation_id = @operation_id;
