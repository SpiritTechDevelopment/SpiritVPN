-- Операции агентам: одновременно журнал исполнения и transactional outbox (§9).

-- Помечает ещё не отправленные операции прежних версий устаревшими (§9).
--
-- IN_FLIGHT намеренно не трогается: такую операцию переводит в SUPERSEDED
-- dispatcher после истечения lease, иначе backend потерял бы след уже отправленного
-- на ноду вызова (§9, решение 3).
-- name: SupersedeStaleOperations :exec
UPDATE agent_operations
SET status = 'SUPERSEDED',
    completed_at = now()
WHERE access_id = $1
  AND desired_version < $2
  AND status IN ('PENDING', 'RETRY_WAIT');

-- Кладёт операцию новой desired version в outbox. Сетевой вызов выполняется
-- dispatcher'ом уже после commit (§9); наличие предыдущей IN_FLIGHT операции
-- откладывает только её dispatch, но не durable создание (§11.1).
--
-- Без ON CONFLICT DO NOTHING: unique (access_id, desired_version) здесь работает как
-- assertion — повтор означал бы, что версия не выросла на изменение desired-кортежа
-- (решение 3).
--
-- next_attempt_at = now(), а не NULL: partial index agent_operations_retryable
-- построен по (next_attempt_at, node_id, operation_id), и строка с NULL не была бы
-- выбрана dispatcher'ом как готовая к отправке (решение 10).
-- name: InsertAgentOperation :exec
INSERT INTO agent_operations (
    operation_id, node_id, access_id, operation_type, desired_version,
    status, next_attempt_at
) VALUES ($1, $2, $3, $4, $5, 'PENDING', now());
