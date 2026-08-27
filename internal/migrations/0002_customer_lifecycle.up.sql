-- Administrative lifecycle and durable deletion tombstones.

ALTER TABLE customer_entitlements
    ADD COLUMN lifecycle_state text NOT NULL DEFAULT 'ACTIVE'
        CHECK (lifecycle_state IN ('ACTIVE', 'BLOCKED', 'DELETING', 'DELETED')),
    ADD COLUMN last_command_fingerprint bytea,
    ADD COLUMN delete_not_before timestamptz,
    ADD COLUMN deleted_at timestamptz;

ALTER TABLE customer_entitlements
    ALTER COLUMN vpn_fleet_id DROP NOT NULL,
    ALTER COLUMN expires_at DROP NOT NULL;

ALTER TABLE customer_entitlements
    ADD CONSTRAINT customer_entitlements_lifecycle_shape CHECK (
        (lifecycle_state = 'DELETED'
            AND vpn_fleet_id IS NULL
            AND expires_at IS NULL
            AND deleted_at IS NOT NULL)
        OR
        (lifecycle_state <> 'DELETED'
            AND vpn_fleet_id IS NOT NULL
            AND expires_at IS NOT NULL
            AND deleted_at IS NULL)
    );

DROP INDEX customer_entitlements_expiry;
CREATE INDEX customer_entitlements_expiry
    ON customer_entitlements (expires_at, customer_id)
    WHERE lifecycle_state = 'ACTIVE';

CREATE INDEX customer_entitlements_deletion
    ON customer_entitlements (delete_not_before, customer_id)
    WHERE lifecycle_state = 'DELETING';
