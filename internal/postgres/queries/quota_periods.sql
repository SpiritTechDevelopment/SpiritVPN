-- Периоды учёта квоты.

-- Блокирует текущий период — единственный с closed_at IS NULL.
-- Отсутствие строки у существующего customer нарушает инвариант.
-- name: LockOpenQuotaPeriod :one
SELECT *
FROM quota_periods
WHERE customer_id = $1
  AND closed_at IS NULL
FOR UPDATE;

-- Закрывает текущий период при renewal. closed_at берётся из now(), то есть из
-- момента начала транзакции; ровно то же значение получает started_at нового
-- периода, поэтому периоды образуют полуоткрытые интервалы [started_at, closed_at)
-- без пересечения и без дыры, в которую провалилась бы дельта трафика.
-- name: CloseOpenQuotaPeriod :exec
UPDATE quota_periods
SET closed_at = now()
WHERE customer_id = $1
  AND closed_at IS NULL;

-- Открывает новый период. Partial unique index quota_periods_one_open_per_customer
-- превращает пропущенный CloseOpenQuotaPeriod в ошибку транзакции, а не в двух
-- открытых периодов сразу.
-- name: InsertQuotaPeriod :exec
INSERT INTO quota_periods (
    quota_period_id, customer_id, started_at, usage_quota_bytes
) VALUES ($1, $2, now(), $3);

-- Меняет лимит открытого периода без сброса накопленного расхода.
-- name: UpdateQuotaPeriodQuota :exec
UPDATE quota_periods
SET usage_quota_bytes = $2
WHERE quota_period_id = $1;
