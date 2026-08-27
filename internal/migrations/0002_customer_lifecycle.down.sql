DROP INDEX customer_entitlements_expiry;
DROP INDEX customer_entitlements_deletion;

DELETE FROM customer_entitlements
WHERE lifecycle_state = 'DELETED';

ALTER TABLE customer_entitlements
    DROP CONSTRAINT customer_entitlements_lifecycle_shape,
    ALTER COLUMN vpn_fleet_id SET NOT NULL,
    ALTER COLUMN expires_at SET NOT NULL,
    DROP COLUMN deleted_at,
    DROP COLUMN delete_not_before,
    DROP COLUMN last_command_fingerprint,
    DROP COLUMN lifecycle_state;

CREATE INDEX customer_entitlements_expiry
    ON customer_entitlements (expires_at, customer_id);
