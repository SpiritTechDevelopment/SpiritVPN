-- Публичный каталог нод из актуального infrastructure manifest.
--
-- Доступность здесь означает только текущую проекцию manifest: состояние
-- customer, access, квоты и наблюдаемая доступность node-agent не участвуют.
-- INNER JOIN намеренно исключает пустые fleets и ноды без актуального
-- membership. Одна нода может состоять в нескольких fleets и тогда возвращается
-- в каждом из них.
-- name: ListAvailableNodes :many
SELECT membership.vpn_fleet_id,
       node.node_id,
       coalesce(node.public_config->>'display_name', '')::text AS display_name
FROM vpn_fleet_nodes membership
JOIN vpn_fleets fleet
  ON fleet.vpn_fleet_id = membership.vpn_fleet_id
 AND fleet.current
JOIN vpn_nodes node
  ON node.node_id = membership.node_id
 AND node.current
WHERE membership.current
ORDER BY membership.vpn_fleet_id, node.node_id;
