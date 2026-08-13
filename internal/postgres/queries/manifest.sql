-- Приём infrastructure manifest и его проекция в таблицы топологии.
--
-- Весь снапшот применяется одной транзакцией: атомарно
-- или не применяется вообще». Физических удалений здесь нет ни одного — строки
-- помечаются current = false и живут вечно.

-- Сериализует приём манифеста: одновременно применяется не более одного снапшота.
--
-- Приём манифеста читает всю проекцию и тут же её переписывает, поэтому без
-- сериализации два параллельных вызова могли бы прочитать одну и ту же
-- last_revision и разъехаться в решениях о destructive. Advisory lock снимается
-- вместе с транзакцией и берётся до первого чтения.
--
-- Цена нулевая: манифест применяет CI/CD инфраструктуры, это редкая операция, а
-- команды customer этот lock не берут и не ждут.
-- name: LockManifestIngest :exec
SELECT pg_advisory_xact_lock(@lock_key::bigint);

-- Последняя принятая revision и её canonical digest. Ноль строк означает,
-- что манифест не принимался ни разу.
-- name: GetLastManifestRevision :one
SELECT revision, digest
FROM manifest_revisions
ORDER BY revision DESC
LIMIT 1;

-- Ноды текущего снапшота: из них выводится, какие ноды из него исчезли.
-- name: ListCurrentNodeIDs :many
SELECT node_id
FROM vpn_nodes
WHERE current
ORDER BY node_id;

-- ВСЕ принятые fleet, а не только текущие: принятый vpn_fleet_id не удаляется и
-- обязан присутствовать в каждом следующем снапшоте.
-- name: ListAcceptedFleetIDs :many
SELECT vpn_fleet_id
FROM vpn_fleets
ORDER BY vpn_fleet_id;

-- name: ListCurrentFleetMemberships :many
SELECT vpn_fleet_id, node_id
FROM vpn_fleet_nodes
WHERE current
ORDER BY vpn_fleet_id, node_id;

-- ВСЕ связи, включая удалённые: routing_key занят навсегда, и оживление его с
-- другой парой (entry, exit) запрещено.
-- name: ListAllBridgeRoutes :many
SELECT vpn_fleet_id, routing_key, entry_node_id, exit_node_id, current
FROM vpn_bridge_routes
ORDER BY vpn_fleet_id, routing_key;

-- Журнал принятых снапшотов. Вставка без ON CONFLICT: повтор revision сюда не
-- доходит (идемпотентный отсекается раньше, конфликтующий отклоняется), поэтому
-- нарушение первичного ключа означало бы сбой сериализации, а не штатный повтор.
-- name: InsertManifestRevision :exec
INSERT INTO manifest_revisions (revision, digest, canonical_payload)
VALUES ($1, $2, $3);

-- Апсерт ноды. Идентичность (node_id) стабильна, endpoint и публичные параметры
-- изменяемы между revisions.
--
-- desired_revision НЕ трогается: приём манифеста не меняет состав desired-юзеров
-- ноды и не создаёт agent operations. Ноду, вернувшуюся в
-- манифест, апсерт оживляет на месте — current снова true.
-- name: UpsertVpnNode :exec
INSERT INTO vpn_nodes (node_id, agent_config, public_config, manifest_revision, current)
VALUES ($1, $2, $3, $4, true)
ON CONFLICT (node_id) DO UPDATE
SET agent_config      = EXCLUDED.agent_config,
    public_config     = EXCLUDED.public_config,
    manifest_revision = EXCLUDED.manifest_revision,
    current           = true,
    updated_at        = now();

-- name: UpsertVpnFleet :exec
INSERT INTO vpn_fleets (vpn_fleet_id, manifest_revision, current)
VALUES ($1, $2, true)
ON CONFLICT (vpn_fleet_id) DO UPDATE
SET manifest_revision = EXCLUDED.manifest_revision,
    current           = true,
    updated_at        = now();

-- name: UpsertFleetNode :exec
INSERT INTO vpn_fleet_nodes (vpn_fleet_id, node_id, manifest_revision, current)
VALUES ($1, $2, $3, true)
ON CONFLICT (vpn_fleet_id, node_id) DO UPDATE
SET manifest_revision = EXCLUDED.manifest_revision,
    current           = true;

-- Апсерт связи. entry_node_id и exit_node_id намеренно НЕ обновляются: пара
-- неизменяема для существующего routing_key. Домен это проверяет и
-- отклоняет манифест раньше; здесь то же правило закреплено структурно, чтобы
-- будущая дыра в проверке не переставила молча входную ноду у выданных access.
-- name: UpsertBridgeRoute :exec
INSERT INTO vpn_bridge_routes (
    vpn_fleet_id, routing_key, entry_node_id, exit_node_id,
    egress_tag, display_name, manifest_revision, current
) VALUES ($1, $2, $3, $4, $5, $6, $7, true)
ON CONFLICT (vpn_fleet_id, routing_key) DO UPDATE
SET egress_tag        = EXCLUDED.egress_tag,
    display_name      = EXCLUDED.display_name,
    manifest_revision = EXCLUDED.manifest_revision,
    current           = true;

-- Ноды, глобально исчезнувшие из манифеста. manifest_revision не трогается
-- и остаётся последней revision, в которой нода присутствовала.
-- name: RetireNodes :exec
UPDATE vpn_nodes
SET current    = false,
    updated_at = now()
WHERE node_id = ANY(@node_ids::text[]);

-- Два параллельных массива сшиваются по WITH ORDINALITY, а не многоаргументным
-- unnest: последний анализатор sqlc не разбирает.
-- name: RetireFleetNodes :exec
UPDATE vpn_fleet_nodes AS membership
SET current = false
WHERE (membership.vpn_fleet_id, membership.node_id) IN (
    SELECT fleets.vpn_fleet_id, nodes.node_id
    FROM unnest(@fleet_ids::bigint[]) WITH ORDINALITY AS fleets(vpn_fleet_id, position)
    JOIN unnest(@node_ids::text[]) WITH ORDINALITY AS nodes(node_id, position)
      ON fleets.position = nodes.position
);

-- name: RetireBridgeRoutes :exec
UPDATE vpn_bridge_routes AS route
SET current = false
WHERE (route.vpn_fleet_id, route.routing_key) IN (
    SELECT fleets.vpn_fleet_id, keys.routing_key
    FROM unnest(@fleet_ids::bigint[]) WITH ORDINALITY AS fleets(vpn_fleet_id, position)
    JOIN unnest(@routing_keys::text[]) WITH ORDINALITY AS keys(routing_key, position)
      ON fleets.position = keys.position
);

-- Durable-джоба fan-out customer access. Ставится в той же транзакции,
-- что и проекция: иначе принятый снапшот остался бы без материализации.
-- name: InsertMaterializationJob :exec
INSERT INTO manifest_materialization_jobs (revision, status)
VALUES ($1, 'PENDING');

-- Запись аудита. sanitized_metadata не содержит секретов и customer ID.
-- name: InsertAuditEvent :exec
INSERT INTO audit_events (
    actor_type, actor_id, action, target_type, target_id,
    request_id, outcome, sanitized_metadata
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8);
