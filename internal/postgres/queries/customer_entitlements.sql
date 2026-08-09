-- Запросы к корневой строке customer (§5, §11.1).

-- Блокирует корневую строку customer — первый шаг любой транзакции, меняющей
-- состояние одного customer (нормативный порядок блокировок, §11.1). Отсутствие
-- строки означает нового customer.
-- name: LockCustomerEntitlement :one
SELECT *
FROM customer_entitlements
WHERE customer_id = $1
FOR UPDATE;

-- Создаёт корневую строку первым успешным Apply (§5). Вставка, а не upsert:
-- отсутствие строки уже установлено под lock, а конфликт по первичному ключу
-- означал бы, что сериализация на корневой строке не сработала, и такое обязано
-- провалить команду, а не тихо перезаписать чужое состояние.
-- name: InsertCustomerEntitlement :exec
INSERT INTO customer_entitlements (
    customer_id, vpn_fleet_id, expires_at, desired_version, last_command_number
) VALUES ($1, $2, $3, $4, $5);

-- Фиксирует принятую команду существующего customer. last_command_number
-- двигается при успешном commit, в том числе на валидном no-op (§5, правило 3);
-- отклонённые команды сюда не доходят. vpn_fleet_id не обновляется: смена fleet в
-- v1 запрещена и отсекается доменом раньше (§5, правило 5).
-- name: UpdateCustomerEntitlement :exec
UPDATE customer_entitlements
SET expires_at = $2,
    desired_version = $3,
    last_command_number = $4,
    updated_at = now()
WHERE customer_id = $1;
