-- Снимок состояния БД для метрик §15.
--
-- Запросы читающие, вне транзакции и намеренно без блокировок: снимок ничего не
-- решает, а согласованность между его частями никому не нужна — каждое значение
-- уезжает в свой независимый gauge.
--
-- Их не вызывает scrape. Их вызывает воркер снимка на своём интервале, а
-- /metrics отдаёт закешированный результат: иначе стоимость наблюдения задавал бы
-- Prometheus снаружи процесса, а отказ базы превращался бы в потерю метрик ровно
-- тогда, когда они нужны.
--
-- EXTRACT(EPOCH FROM ...) везде кастится в double precision явно: в PostgreSQL 14
-- он возвращает numeric, и без каста sqlc вывел бы pgtype.Numeric на величину,
-- которая по смыслу секунды с дробью.

-- Agent operations по статусу: сколько и насколько стара самая старая (§15).
--
-- Возраст осмыслен для PENDING и RETRY_WAIT — это и есть застрявшая очередь. Для
-- терминальных статусов он растёт вечно, потому что строки не удаляются; alert'ы
-- вешаются на первые два.
-- name: StatsAgentOperations :many
SELECT status,
       count(*)::bigint AS count,
       EXTRACT(EPOCH FROM now() - min(created_at))::double precision AS oldest_age_seconds
FROM agent_operations
GROUP BY status;

-- Access по паре состояний (§15).
--
-- Ретайрнутые исключены: они уже не часть желаемого состояния, и их apply_state —
-- история (§4). Индекса под эту группировку нет и не заводится: на масштабе v1
-- (≤2000 users/node) это seq scan в единицы миллисекунд раз в интервал снимка.
-- name: StatsAccesses :many
SELECT desired_state, apply_state, count(*)::bigint AS count
FROM vpn_accesses
WHERE retired_at IS NULL
GROUP BY desired_state, apply_state;

-- Состояние опроса по каждой текущей ноде (§12, §15).
--
-- Backend не знает, сколько batch лежит в спуле агента неподтверждёнными: это
-- состояние ноды, и узнать его можно только спросив. Здесь честное приближение —
-- время с последней попытки опроса и подтверждённая позиция. Растущий возраст
-- при неподвижном acked_sequence и есть backlog.
-- name: StatsNodeCursors :many
SELECT c.node_id,
       EXTRACT(EPOCH FROM now() - c.updated_at)::double precision AS last_pull_age_seconds,
       c.acked_sequence,
       -- Только протухший lease: занятый — штатная работа, и отдельной серией на
       -- каждую ноду он ничего не добавляет к агрегату по воркеру.
       (c.lease_expires_at IS NOT NULL AND c.lease_expires_at < now())::boolean AS lease_expired
FROM node_usage_cursors c
JOIN vpn_nodes n ON n.node_id = c.node_id AND n.current;

-- Карантинные usage-items по причине (§12, §15).
--
-- Это счётчик строк таблицы, а не событий: карантин наблюдаем только через неё, и
-- собственного счётчика в процессе у него нет. Значение растёт монотонно до
-- ретенции, поэтому alert строится на rate(), а не на абсолютном числе.
-- name: StatsQuarantine :many
SELECT reason, count(*)::bigint AS count
FROM traffic_batch_quarantine
GROUP BY reason;

-- Скаляры одним round trip.
--
-- Подзапросами, а не отдельными запросами: каждый из них — тривиальный агрегат, и
-- девять походов в базу вместо одного ничего бы не дали, кроме девяти шансов
-- отказать по-разному.
--
-- schema_migrations здесь нет: таблицу заводит golang-migrate, в internal/migrations
-- её нет, и sqlc о ней не знает. Её читает адаптер сырым запросом — так же, как это
-- уже делает readiness-проверка.
-- name: StatsScalars :one
WITH due AS (
    -- Тот же предикат, что и у LockNextDueExpiredCustomer: метрика обязана
    -- считать ровно ту очередь, которую разбирает воркер. Условие на PRESENT
    -- существенно — без него уже погашенные customer оставались бы просроченными
    -- навсегда и лаг рос бы вечно при исправном воркере.
    SELECT e.expires_at
    FROM customer_entitlements e
    WHERE e.expires_at <= now()
      AND EXISTS (
          SELECT 1
          FROM vpn_accesses a
          WHERE a.customer_id = e.customer_id
            AND a.desired_state = 'PRESENT'
            AND a.retired_at IS NULL
      )
)
SELECT
    (SELECT coalesce(max(revision), 0) FROM manifest_revisions)::bigint
        AS manifest_revision,

    (SELECT coalesce(max(revision), 0) FROM manifest_materialization_jobs WHERE status = 'DONE')::bigint
        AS materialized_revision,

    -- Ноль означает «незавершённых джоб нет», а не «лаг нулевой». Разница видна по
    -- сравнению manifest_revision с materialized_revision.
    (SELECT coalesce(EXTRACT(EPOCH FROM now() - min(updated_at)), 0)
     FROM manifest_materialization_jobs
     WHERE status IN ('PENDING', 'IN_PROGRESS'))::double precision
        AS materialization_lag_seconds,

    (SELECT count(*) FROM due)::bigint
        AS expired_customers,

    (SELECT coalesce(EXTRACT(EPOCH FROM now() - min(expires_at)), 0) FROM due)::double precision
        AS expiry_lag_seconds,

    -- Только открытые периоды: exhausted_at закрытого — история, а не действующая
    -- блокировка (§12).
    (SELECT count(*)
     FROM node_quota_usage u
     JOIN quota_periods p ON p.quota_period_id = u.quota_period_id
     WHERE p.closed_at IS NULL AND u.exhausted_at IS NOT NULL)::bigint
        AS exhausted_node_quotas,

    -- Lease диспетчера. Оба условия ограничены IN_FLIGHT: завершение операции
    -- обнуляет lease-колонки, но партиальный индекс есть только на этот статус.
    (SELECT count(*) FROM agent_operations
     WHERE status = 'IN_FLIGHT' AND lease_expires_at >= now())::bigint
        AS dispatch_leases_held,
    (SELECT count(*) FROM agent_operations
     WHERE status = 'IN_FLIGHT' AND lease_expires_at < now())::bigint
        AS dispatch_leases_expired,

    (SELECT count(*) FROM manifest_materialization_jobs WHERE lease_expires_at >= now())::bigint
        AS materialize_leases_held,
    (SELECT count(*) FROM manifest_materialization_jobs WHERE lease_expires_at < now())::bigint
        AS materialize_leases_expired,

    (SELECT count(*) FROM node_usage_cursors WHERE lease_expires_at >= now())::bigint
        AS usage_leases_held,
    (SELECT count(*) FROM node_usage_cursors WHERE lease_expires_at < now())::bigint
        AS usage_leases_expired;
