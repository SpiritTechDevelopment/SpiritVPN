-- Administrative lifecycle and physical deletion.

-- name: InsertDeletedCustomerTombstone :exec
INSERT INTO customer_entitlements (
    customer_id, vpn_fleet_id, expires_at, desired_version,
    last_command_number, last_command_fingerprint, lifecycle_state, deleted_at
) VALUES ($1, NULL, NULL, 0, $2, $3, 'DELETED', now());

-- name: LockNextDeletionCandidate :one
SELECT *
FROM customer_entitlements
WHERE lifecycle_state = 'DELETING'
  AND delete_not_before <= now()
  AND NOT EXISTS (
      SELECT 1
      FROM vpn_accesses a
      WHERE a.customer_id = customer_entitlements.customer_id
        AND (a.desired_state <> 'ABSENT' OR a.apply_state <> 'APPLIED')
  )
  AND NOT EXISTS (
      SELECT 1
      FROM agent_operations o
      JOIN vpn_accesses a ON a.access_id = o.access_id
      WHERE a.customer_id = customer_entitlements.customer_id
        AND o.status IN ('PENDING', 'RETRY_WAIT', 'IN_FLIGHT')
  )
ORDER BY delete_not_before, customer_id
FOR UPDATE SKIP LOCKED
LIMIT 1;

-- true означает, что каждый текущий access подтверждён как ABSENT и ни одна
-- актуальная операция ещё не выполняется/не ждёт.
-- name: CustomerDeletionReady :one
SELECT NOT EXISTS (
    SELECT 1
    FROM vpn_accesses a
    WHERE a.customer_id = $1
      AND (a.desired_state <> 'ABSENT' OR a.apply_state <> 'APPLIED')
) AND NOT EXISTS (
    SELECT 1
    FROM agent_operations o
    JOIN vpn_accesses a ON a.access_id = o.access_id
    WHERE a.customer_id = $1
      AND o.status IN ('PENDING', 'RETRY_WAIT', 'IN_FLIGHT')
)::boolean AS ready;

-- name: DeleteCustomerQuarantine :exec
DELETE FROM traffic_batch_quarantine q
USING vpn_accesses a
WHERE a.customer_id = $1
  AND q.accounting_id = a.accounting_id;

-- name: DeleteCustomerProcessedUsage :exec
DELETE FROM traffic_usage_items_processed p
WHERE p.access_id IN (
          SELECT a.access_id FROM vpn_accesses a WHERE a.customer_id = $1
      )
   OR p.quota_period_id IN (
          SELECT qp.quota_period_id FROM quota_periods qp WHERE qp.customer_id = $1
      )
   OR p.accounting_id IN (
          SELECT a.accounting_id FROM vpn_accesses a WHERE a.customer_id = $1
      );

-- name: DeleteCustomerAgentOperations :exec
DELETE FROM agent_operations o
USING vpn_accesses a
WHERE a.customer_id = $1
  AND o.access_id = a.access_id;

-- name: DeleteCustomerNodeUsage :exec
DELETE FROM node_quota_usage u
USING quota_periods p
WHERE p.customer_id = $1
  AND u.quota_period_id = p.quota_period_id;

-- name: DeleteCustomerQuotaPeriods :exec
DELETE FROM quota_periods WHERE customer_id = $1;

-- name: DeleteCustomerAccesses :exec
DELETE FROM vpn_accesses WHERE customer_id = $1;

-- name: FinalizeCustomerTombstone :exec
UPDATE customer_entitlements
SET vpn_fleet_id = NULL,
    expires_at = NULL,
    lifecycle_state = 'DELETED',
    delete_not_before = NULL,
    deleted_at = now(),
    updated_at = now()
WHERE customer_id = $1
  AND lifecycle_state = 'DELETING';
