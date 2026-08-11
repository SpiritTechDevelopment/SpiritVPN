-- Учёт трафика через pull от агента (§12).
--
-- Транзакций у шага три, и между первой и остальными идёт сетевой вызов: §11.1
-- запрещает держать транзакцию открытой во время обращения к агенту. Batch
-- обрабатывается не одной большой транзакцией, а группами
-- (customer_id, node_id, quota_period_id) — по короткой транзакции на группу.

-- Заводит строки курсора всем текущим нодам (§12).
--
-- Отдельным оператором перед claim, потому что claim обязан брать строку под
-- FOR UPDATE SKIP LOCKED, а несуществующую строку заблокировать нельзя. Вставка
-- идемпотентна и почти всегда не делает ничего.
--
-- updated_at = 'epoch', а не now(): колонка означает «последняя попытка pull»
-- (решение 61), и новая нода обязана опрашиваться сразу, а не через интервал.
-- spool_id пустой — агент игнорирует подтверждение чужого spool_id, поэтому первый
-- pull безопасно уходит с ним.
-- name: EnsureUsageCursors :exec
INSERT INTO node_usage_cursors (node_id, spool_id, updated_at)
SELECT node_id, '', 'epoch'
FROM vpn_nodes
WHERE current
ON CONFLICT (node_id) DO NOTHING;

-- Берёт lease ноды, которую пора опросить (§12, решение 60).
--
-- Условие на lease разрешает три случая: ничей, чужой протухший (воркер упал) и
-- собственный. Последнее нужно, потому что каждый шаг идёт отдельной транзакцией.
--
-- updated_at гейтит темп: чаще, чем агент опрашивает Xray, дёргать нечего — он
-- вернёт пустой ответ (§12, решение 61). Он же задаёт порядок, поэтому давно не
-- опрошенная нода идёт первой и ни одна не голодает.
--
-- Ноды с current = false не опрашиваются: инфраструктура их уже погасила (§6).
-- name: ClaimUsageNode :one
UPDATE node_usage_cursors
SET lease_owner      = @owner::text,
    lease_expires_at = now() + make_interval(secs => @lease_seconds::int),
    updated_at       = now()
WHERE node_id = (
    SELECT c.node_id
    FROM node_usage_cursors c
    JOIN vpn_nodes n ON n.node_id = c.node_id AND n.current
    WHERE (c.lease_expires_at IS NULL
           OR c.lease_expires_at < now()
           OR c.lease_owner = @owner::text)
      AND c.updated_at <= now() - make_interval(secs => @min_interval_seconds::int)
    ORDER BY c.updated_at, c.node_id
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING node_id, spool_id, acked_sequence,
          (SELECT agent_config FROM vpn_nodes WHERE node_id = node_usage_cursors.node_id) AS agent_config;

-- Снимает lease по завершении шага.
--
-- Без явного снятия нода простаивала бы до истечения TTL: он с запасом перекрывает
-- весь шаг вместе с RPC и потому заметно длиннее интервала опроса.
--
-- Условие на владельца обязательно: чужой lease снимать нельзя, иначе воркер,
-- задержавшийся на своём шаге, потерял бы ноду в пользу второго.
-- name: ReleaseUsageLease :exec
UPDATE node_usage_cursors
SET lease_owner      = NULL,
    lease_expires_at = NULL
WHERE node_id = @node_id::text
  AND lease_owner = @owner::text;

-- Сопоставляет accounting_id с их владельцами (§12, шаг 2).
--
-- Ретайрнутые и погашенные access тоже возвращаются: §12 прямо разрешает учитывать
-- items после expiry/retire, а accounting_id уникален и не переиспользуется (§4),
-- поэтому исторический маппинг — это просто чтение vpn_accesses без фильтров.
-- Ненайденные accounting_id уходят в карантин (§12, шаг 6).
-- name: ResolveAccountingIDs :many
SELECT accounting_id, access_id, customer_id, entry_node_id
FROM vpn_accesses
WHERE accounting_id = ANY (@accounting_ids::text[]);

-- Блокирует период, в который попадает момент сбора batch (§12).
--
-- Границы полуоткрыты: started_at <= collected_at < closed_at. Открытый период
-- закрывающей границы не имеет. Ноль строк означает, что подходящего периода нет
-- вовсе, — такой item уходит в карантин, а не в IGNORED_CLOSED_PERIOD, потому что
-- период не закрыт, его не существует (решение 65).
-- name: LockQuotaPeriodAt :one
SELECT quota_period_id, customer_id, started_at, closed_at, usage_quota_bytes
FROM quota_periods
WHERE customer_id = @customer_id::text
  AND started_at <= @collected_at
  AND (closed_at IS NULL OR @collected_at < closed_at)
FOR UPDATE;

-- Заводит строку расхода ноды, если её ещё нет (§12).
--
-- Отдельный запрос, а не InsertNodeQuotaUsage: там отсутствие строки является
-- инвариантом и конфликт обязан провалить команду, а здесь наоборот — строки
-- может не быть штатно. Истёкшему customer материализация их не создаёт (§13), а
-- начислять его items §12 всё равно требует, и без строки байты потерялись бы.
-- name: EnsureNodeQuotaUsageRow :exec
INSERT INTO node_quota_usage (quota_period_id, node_id)
VALUES (@quota_period_id, @node_id::text)
ON CONFLICT (quota_period_id, node_id) DO NOTHING;

-- Блокирует строку расхода одной ноды в периоде (§11.1, шаг 3).
--
-- Именно этот row lock сериализует несколько access одного customer на одной ноде,
-- поэтому порог активируется ровно один раз (§12).
-- name: LockNodeQuotaUsageRow :one
SELECT node_id, total_bytes, exhausted_at
FROM node_quota_usage
WHERE quota_period_id = @quota_period_id
  AND node_id = @node_id::text
FOR UPDATE;

-- Регистрирует items в реестре идемпотентности и возвращает только НОВЫЕ
-- (§12, шаг 4).
--
-- ON CONFLICT DO NOTHING RETURNING — это и есть дедуп: попадание означает, что item
-- уже начислен, и повторный pull, перезапуск воркера или повтор batch его не
-- удвоят. Начисляются counters только по возвращённым строкам.
-- name: RegisterProcessedUsageItems :many
INSERT INTO traffic_usage_items_processed (
    node_id, spool_id, sequence, accounting_id, access_id, quota_period_id, result
)
-- Параллельные массивы сшиваются через WITH ORDINALITY: многоаргументный unnest
-- разбирается только PostgreSQL, но не парсером sqlc.
SELECT @node_id::text, @spool_id::text, @sequence::numeric,
       a.accounting_id, i.access_id, @quota_period_id::uuid, @result::text
FROM unnest(@accounting_ids::text[]) WITH ORDINALITY AS a(accounting_id, ord)
JOIN unnest(@access_ids::uuid[]) WITH ORDINALITY AS i(access_id, ord) ON i.ord = a.ord
ON CONFLICT DO NOTHING
RETURNING accounting_id;

-- Регистрирует карантинные items как обработанные (§12, шаг 6).
--
-- access_id и quota_period_id остаются NULL: у неизвестного accounting_id владельца
-- нет, а у item без подходящего периода — периода. Отметка нужна, чтобы один плохой
-- item не блокировал batch навсегда.
-- name: RegisterQuarantinedUsageItems :exec
INSERT INTO traffic_usage_items_processed (
    node_id, spool_id, sequence, accounting_id, result
)
SELECT @node_id::text, @spool_id::text, @sequence::numeric, accounting_id, 'QUARANTINED'
FROM unnest(@accounting_ids::text[]) AS accounting_id
ON CONFLICT DO NOTHING;

-- Кладёт items в карантин (§12, шаг 6).
--
-- sanitized_payload собирается ЗДЕСЬ, а не приходит из Go: так в него физически не
-- может попасть ничего, кроме счётчиков байтов. Ни client_uuid, ни IP, ни
-- назначение в usage не приходят вовсе (§12), но карантин — последнее место, где
-- стоит полагаться на дисциплину вызывающего.
-- name: QuarantineUsageItems :exec
INSERT INTO traffic_batch_quarantine (
    node_id, spool_id, sequence, accounting_id, reason, sanitized_payload
)
SELECT @node_id::text, @spool_id::text, @sequence::numeric, a.accounting_id, @reason::text,
       jsonb_build_object('uplink_bytes', u.uplink, 'downlink_bytes', d.downlink)
FROM unnest(@accounting_ids::text[]) WITH ORDINALITY AS a(accounting_id, ord)
JOIN unnest(@uplinks::numeric[]) WITH ORDINALITY AS u(uplink, ord) ON u.ord = a.ord
JOIN unnest(@downlinks::numeric[]) WITH ORDINALITY AS d(downlink, ord) ON d.ord = a.ord;

-- Добавляет дельты к counters ноды (§12, шаг 4).
--
-- Именно прибавление, а не запись: items приходят дельтами за интервал опроса, и
-- агент — единственный владелец reset'а Xray-счётчиков. total_bytes считается
-- generated-колонкой и здесь не пишется.
-- name: AddNodeQuotaUsage :exec
UPDATE node_quota_usage
SET uplink_bytes   = uplink_bytes + @uplink_bytes,
    downlink_bytes = downlink_bytes + @downlink_bytes,
    updated_at     = now()
WHERE quota_period_id = @quota_period_id
  AND node_id = @node_id::text;

-- Все нератайрнутые access customer на одной входной ноде (§12, шаг 5).
--
-- §12 требует гасить их ВСЕ, а не только тот, чей accounting_id приехал в batch:
-- квота применяется к ноде целиком, а трафик всех FREEDOM и BRIDGE этого customer
-- на ней суммируется в один node quota period (§4).
--
-- Без FOR UPDATE: корневая строка customer уже заблокирована, поэтому
-- конкурирующего writer'а этих строк не существует (§11.1).
-- name: ListCustomerNodeAccesses :many
SELECT access_id, kind, logical_target_key, generation, entry_node_id,
       egress_key, accounting_id, desired_state, desired_version, retired_at
FROM vpn_accesses
WHERE customer_id = @customer_id::text
  AND entry_node_id = @entry_node_id::text
  AND retired_at IS NULL
ORDER BY access_id;

-- Сдвигает подтверждённую позицию спула (§12).
--
-- Вызывается только после durable commit ВСЕХ групп batch: агент удаляет из спула
-- ровно подтверждённое, и опережающее подтверждение потеряло бы неучтённый трафик
-- (решение 63).
--
-- spool_id пишется вместе с sequence: смена спула означает новую нумерацию с нуля,
-- и хранить старый id рядом с новым sequence нельзя (решение 64).
-- name: AdvanceUsageCursor :exec
UPDATE node_usage_cursors
SET spool_id       = @spool_id::text,
    acked_sequence = @acked_sequence,
    updated_at     = now()
WHERE node_id = @node_id::text;
