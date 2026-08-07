-- Запросы к корневой строке customer (§5, §11.1).

-- Блокирует корневую строку customer — первый шаг любой транзакции, меняющей
-- состояние одного customer (нормативный порядок блокировок, §11.1). Отсутствие
-- строки означает нового customer.
-- name: LockCustomerEntitlement :one
SELECT *
FROM customer_entitlements
WHERE customer_id = $1
FOR UPDATE;
