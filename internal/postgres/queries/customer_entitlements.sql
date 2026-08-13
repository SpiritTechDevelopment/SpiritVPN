-- Запросы к корневой строке customer.

-- Блокирует корневую строку customer — первый шаг любой транзакции, меняющей
-- состояние одного customer (нормативный порядок блокировок). Отсутствие
-- строки означает нового customer.
-- name: LockCustomerEntitlement :one
SELECT *
FROM customer_entitlements
WHERE customer_id = $1
FOR UPDATE;

-- Создаёт корневую строку первым успешным Apply. Вставка, а не upsert:
-- отсутствие строки уже установлено под lock, а конфликт по первичному ключу
-- означал бы, что сериализация на корневой строке не сработала, и такое обязано
-- провалить команду, а не тихо перезаписать чужое состояние.
-- name: InsertCustomerEntitlement :exec
INSERT INTO customer_entitlements (
    customer_id, vpn_fleet_id, expires_at, desired_version, last_command_number
) VALUES ($1, $2, $3, $4, $5);

-- Фиксирует принятую команду существующего customer. last_command_number
-- двигается при успешном commit, в том числе на валидном no-op;
-- отклонённые команды сюда не доходят. vpn_fleet_id не обновляется: смена fleet в
-- v1 запрещена и отсекается доменом раньше.
-- name: UpdateCustomerEntitlement :exec
UPDATE customer_entitlements
SET expires_at = $2,
    desired_version = $3,
    last_command_number = $4,
    updated_at = now()
WHERE customer_id = $1;
