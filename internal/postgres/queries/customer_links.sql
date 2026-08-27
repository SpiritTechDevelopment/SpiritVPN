-- Чтение ссылок customer: GetCustomerAccessLinks и сборка VLESS URI.
--
-- Путь read-only: ни одного FOR UPDATE. Все транзакции, меняющие состояние
-- customer, сериализуются на его корневой строке, а этот запрос ничего
-- не меняет, поэтому единственное, что ему нужно, — согласованность двух
-- операторов между собой; её даёт снимок транзакции, а не блокировки.

-- Срок действия customer и время того же снимка одним оператором: обе величины
-- сравниваются друг с другом, и брать их из разных моментов нельзя:
-- TIME_EXPIRED выводится из current_time >= expires_at.
--
-- Ноль строк означает неизвестного customer — NOT_FOUND. Пустой список
-- ссылок у существующего customer таким исходом не является, поэтому запрос
-- отдельный, а не объединён с выборкой ссылок.
-- name: GetCustomerLinksHeader :one
SELECT now()::timestamptz AS tx_now,
       expires_at,
       lifecycle_state
FROM customer_entitlements
WHERE customer_id = $1;

-- Все текущие ссылки customer в фиксированном порядке ответа.
--
-- Из выборки исключены:
--
--   * ретайрнутые access — historical наружу не отдаётся;
--   * access, чья логическая цель отсутствует в текущем manifest, — по ней URI
--     запрещена, а показать её нечем: PENDING обещал бы ссылку, которая не
--     появится, а BLOCKED требует причину, которой в контракте нет.
--     Такой access ретайрит materialization job, но между commit манифеста и
--     её отработкой существует окно, ради которого фильтр и нужен;
--   * access на входной ноде, глобально удалённой из manifest: backend прекращает
--     выдавать её ссылки.
--
-- Ветка цели ровно одна на строку: kind в условии join'а гарантирует, что
-- FREEDOM сопоставляется с membership ноды, а BRIDGE — со связью, и обе сразу
-- совпасть не могут.
--
-- Порядок сортировки — внутренний: (kind, logical_target_key, access_id).
-- Значения kind ('BRIDGE', 'FREEDOM') — ASCII в верхнем регистре, их взаимный
-- порядок одинаков в любой collation.
-- name: ListCustomerAccessLinks :many
SELECT a.kind,
       a.desired_state,
       a.apply_state,
       a.encrypted_client_uuid,
       a.encryption_key_id,
       entry.public_config                     AS entry_public_config,
       br.display_name                         AS bridge_display_name,
       (usage.exhausted_at IS NOT NULL)::bool  AS quota_exhausted,
       coalesce(period.usage_quota_bytes, 0::numeric) AS usage_quota_bytes,
       coalesce(usage.total_bytes, 0::numeric)        AS consumed_bytes
FROM vpn_accesses a
JOIN customer_entitlements e
  ON e.customer_id = a.customer_id
-- Входная нода: для FREEDOM это сама цель, для BRIDGE — entry_node_id связи.
-- Все параметры URI, кроме uuid, берутся именно отсюда.
JOIN vpn_nodes entry
  ON entry.node_id = a.entry_node_id
 AND entry.current
LEFT JOIN vpn_fleet_nodes fleet_node
  ON a.kind = 'FREEDOM'
 AND fleet_node.vpn_fleet_id = e.vpn_fleet_id
 AND fleet_node.node_id = a.logical_target_key
 AND fleet_node.current
LEFT JOIN vpn_bridge_routes br
  ON a.kind = 'BRIDGE'
 AND br.vpn_fleet_id = e.vpn_fleet_id
 AND br.routing_key = a.logical_target_key
 AND br.current
-- Открытый период у customer максимум один (partial unique index). Строка
-- расхода относится к entry_node_id access: для FREEDOM это сама нода, для
-- BRIDGE — входная нода связи. Её отсутствие означает нулевой расход; отсутствие
-- периода возможно только при нарушении инварианта и также проецируется в нули.
LEFT JOIN quota_periods period
  ON period.customer_id = a.customer_id
 AND period.closed_at IS NULL
LEFT JOIN node_quota_usage usage
  ON usage.quota_period_id = period.quota_period_id
 AND usage.node_id = a.entry_node_id
WHERE a.customer_id = $1
  AND a.retired_at IS NULL
  AND (fleet_node.node_id IS NOT NULL OR br.routing_key IS NOT NULL)
ORDER BY a.kind, a.logical_target_key, a.access_id;
