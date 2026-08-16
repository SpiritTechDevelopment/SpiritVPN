-- Откат 0001_baseline.up.sql. Дроп в обратном порядке зависимостей.
-- Расширение pgcrypto намеренно не удаляем: оно может быть общим.

DROP TABLE IF EXISTS audit_events;
DROP TABLE IF EXISTS traffic_batch_quarantine;
DROP TABLE IF EXISTS traffic_usage_items_processed;
DROP TABLE IF EXISTS node_usage_cursors;
DROP TABLE IF EXISTS manifest_materialization_jobs;
DROP TABLE IF EXISTS agent_operations;
DROP TABLE IF EXISTS vpn_accesses;
DROP TABLE IF EXISTS node_quota_usage;
DROP TABLE IF EXISTS quota_periods;
DROP TABLE IF EXISTS customer_entitlements;
DROP TABLE IF EXISTS vpn_bridge_routes;
DROP TABLE IF EXISTS vpn_fleet_nodes;
DROP TABLE IF EXISTS vpn_nodes;
DROP TABLE IF EXISTS vpn_fleets;
DROP TABLE IF EXISTS manifest_revisions;
