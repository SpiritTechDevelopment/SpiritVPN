-- Индивидуальные customer access.

-- Возвращает Все access customer, включая ретайрнутые: без них нельзя вычислить
-- поколение при повторном появлении ранее удалённой цели.
--
-- Без FOR UPDATE: все транзакции, меняющие состояние одного customer, сначала
-- блокируют его корневую строку, поэтому конкурирующего writer'а этих строк
-- на момент чтения не существует. Сами UPDATE ниже берут row locks в порядке
-- access_id, заданном этой сортировкой.
--
-- encrypted_client_uuid здесь не читается: доменные правила его не используют, а
-- лишний вынос секрета в память нужен только там, где он действительно требуется.
-- name: ListCustomerAccesses :many
SELECT access_id, kind, logical_target_key, generation, entry_node_id,
       egress_key, accounting_id, desired_state, desired_version, retired_at
FROM vpn_accesses
WHERE customer_id = $1
ORDER BY access_id;

-- Создаёт недостающий под текущую топологию access. accounting_id и
-- client_uuid выдаются генератором и никогда не переиспользуются; unique-индексы по
-- accounting_id и (customer_id, kind, logical_target_key, generation) здесь работают
-- как assertion — коллизия обязана провалить команду, а не быть проглоченной
-- ON CONFLICT.
--
-- apply_state: PRESENT рождается PENDING (операция выпущена и ещё не доставлена),
-- ABSENT — APPLIED, потому что операции у него нет, а отсутствие юзера уже является
-- состоянием Xray по умолчанию.
-- name: InsertVpnAccess :exec
INSERT INTO vpn_accesses (
    access_id, customer_id, kind, logical_target_key, generation,
    entry_node_id, egress_key, accounting_id, encrypted_client_uuid,
    encryption_key_id, desired_state, apply_state, desired_version
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13);

-- Переводит существующий access в новое desired state. desired_version растёт на
-- изменение desired-кортежа и попадает в ту же транзакцию, что и операция.
-- apply_state сбрасывается в PENDING: доставка нового желаемого
-- состояния ещё не подтверждена.
-- name: UpdateAccessDesiredState :exec
UPDATE vpn_accesses
SET desired_state = $2,
    desired_version = $3,
    apply_state = 'PENDING'
WHERE access_id = $1;
