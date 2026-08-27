-- Запросы к корневой строке customer.

-- Блокирует корневую строку customer — первый шаг любой транзакции, меняющей
-- состояние одного customer (нормативный порядок блокировок). Отсутствие
-- строки означает нового customer.
-- name: LockCustomerEntitlement :one
SELECT *
FROM customer_entitlements
WHERE customer_id = $1
FOR UPDATE;

-- Заводит стабильную точку сериализации до первого command. ON CONFLICT нужен
-- для гонки двух первых транзакций: проигравшая дождётся победителя, затем обе
-- заблокируют одну строку через LockCustomerEntitlement. При последующей ошибке
-- INSERT откатывается вместе со всей командой.
-- name: EnsureCustomerEntitlementRoot :exec
INSERT INTO customer_entitlements (
    customer_id, vpn_fleet_id, expires_at, desired_version,
    last_command_number, lifecycle_state, deleted_at
) VALUES ($1, NULL, NULL, 0, 0, 'DELETED', now())
ON CONFLICT (customer_id) DO NOTHING;

-- Создаёт корневую строку первым успешным Apply. Вставка, а не upsert:
-- отсутствие строки уже установлено под lock, а конфликт по первичному ключу
-- означал бы, что сериализация на корневой строке не сработала, и такое обязано
-- провалить команду, а не тихо перезаписать чужое состояние.
-- name: InsertCustomerEntitlement :exec
INSERT INTO customer_entitlements (
    customer_id, vpn_fleet_id, expires_at, desired_version, last_command_number,
    lifecycle_state, last_command_fingerprint
) VALUES ($1, $2, $3, $4, $5, 'ACTIVE', $6);

-- Фиксирует принятую команду существующего customer. last_command_number
-- двигается при успешном commit, в том числе на валидном no-op;
-- отклонённые команды сюда не доходят. vpn_fleet_id не обновляется: смена fleet в
-- v1 запрещена и отсекается доменом раньше.
-- name: UpdateCustomerEntitlement :exec
UPDATE customer_entitlements
SET expires_at = $2,
    desired_version = $3,
    last_command_number = $4,
    last_command_fingerprint = $5,
    updated_at = now()
WHERE customer_id = $1;

-- Возвращает DELETED tombstone в новую активную подписку. Старые operational
-- строки к этому моменту уже удалены cleanup worker'ом.
-- name: ReactivateCustomerEntitlement :exec
UPDATE customer_entitlements
SET vpn_fleet_id = @vpn_fleet_id,
    expires_at = @expires_at,
    desired_version = @desired_version,
    last_command_number = @last_command_number,
    last_command_fingerprint = @last_command_fingerprint,
    lifecycle_state = 'ACTIVE',
    delete_not_before = NULL,
    deleted_at = NULL,
    updated_at = now()
WHERE customer_id = @customer_id;

-- Фиксирует lifecycle-команду и её ordering token.
-- name: UpdateCustomerLifecycle :exec
UPDATE customer_entitlements
SET lifecycle_state = @lifecycle_state,
    desired_version = @desired_version,
    last_command_number = @last_command_number,
    last_command_fingerprint = @last_command_fingerprint,
    delete_not_before = @delete_not_before,
    updated_at = now()
WHERE customer_id = @customer_id;
