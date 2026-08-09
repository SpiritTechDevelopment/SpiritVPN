-- Чтение текущей проекции топологии fleet и блокировка нод (§6, §11.1).
--
-- Текущая топология — это строки проекции с current = true. Manifest является
-- полным снапшотом, а его валидация требует существования всех node references
-- (§6), поэтому глобально удалённая нода не может остаться ни в актуальном
-- membership, ни в актуальной bridge-связи. Поддерживать этот инвариант обязан
-- срез, применяющий manifest; здесь он не перепроверяется джойнами, чтобы
-- определение «текущей топологии» жило в одном месте.

-- Проверяет, что fleet присутствует в последнем принятом снапшоте (§5, правило 6).
-- Ранее принятый fleet физически не удаляется (§6), поэтому строка может
-- существовать с current = false: приём новых команд для него закрыт, а FK уже
-- привязанных customer не ломается.
-- name: FleetIsCurrent :one
SELECT EXISTS (
    SELECT 1
    FROM vpn_fleets
    WHERE vpn_fleet_id = $1
      AND current
) AS is_current;

-- Ноды fleet. Каждая даёт один FREEDOM access (§4).
-- name: ListCurrentFleetNodes :many
SELECT node_id
FROM vpn_fleet_nodes
WHERE vpn_fleet_id = $1
  AND current
ORDER BY node_id;

-- BRIDGE-связи fleet. Каждая даёт один BRIDGE access на своей входной ноде;
-- egress_tag форвардится агенту дословно (§6, §7).
-- name: ListCurrentFleetBridgeRoutes :many
SELECT routing_key, entry_node_id, exit_node_id, egress_tag
FROM vpn_bridge_routes
WHERE vpn_fleet_id = $1
  AND current
ORDER BY routing_key;

-- Блокирует строки нод, состав desired-юзеров которых меняется (§11.1, шаг 5).
-- ORDER BY внутри FOR UPDATE обязателен: узел блокировки стоит в плане над
-- сортировкой, поэтому строки блокируются в порядке node_id, и транзакции,
-- затрагивающие пересекающиеся наборы нод, не встают в deadlock.
-- name: LockNodesForUpdate :many
SELECT node_id
FROM vpn_nodes
WHERE node_id = ANY(@node_ids::text[])
ORDER BY node_id
FOR UPDATE;

-- Увеличивает desired_revision ровно один раз на ноду независимо от числа
-- затронутых access (§11.1). Вызывается только после LockNodesForUpdate, поэтому
-- порядок строк внутри этого UPDATE уже не важен — все нужные locks взяты.
-- name: BumpNodesDesiredRevision :exec
UPDATE vpn_nodes
SET desired_revision = desired_revision + 1,
    updated_at = now()
WHERE node_id = ANY(@node_ids::text[]);
