-- Расход трафика customer по нодам внутри периода.

-- Блокирует строки расхода в порядке node_id. total_bytes —
-- generated-колонка, поэтому читается, но не пишется.
-- name: LockNodeQuotaUsage :many
SELECT node_id, total_bytes, exhausted_at
FROM node_quota_usage
WHERE quota_period_id = $1
ORDER BY node_id
FOR UPDATE;

-- Заводит нулевые строки расхода всем нодам fleet при открытии периода.
-- Внутри уже открытого периода Apply строк не создаёт: ноде, добавленной в fleet
-- позже, строку создаёт materialization job вместе с её FREEDOM access.
--
-- Строки новые, конфликтующих locks они не берут, поэтому порядок вставки внутри
-- одного оператора значения не имеет.
-- name: InsertNodeQuotaUsage :exec
INSERT INTO node_quota_usage (quota_period_id, node_id)
SELECT @quota_period_id::uuid, node_id
FROM unnest(@node_ids::text[]) AS node_id;

-- Ставит и снимает отметку исчерпания под новый лимит в рамках того же периода.
-- Непустой exhausted_at ставит отметку, NULL снимает её. Значение приходит
-- из now() той же транзакции, поэтому отметка и решение о desired state опираются
-- на один и тот же момент времени.
-- name: SetNodeQuotaExhausted :exec
UPDATE node_quota_usage
SET exhausted_at = $3,
    updated_at = now()
WHERE quota_period_id = $1
  AND node_id = $2;
