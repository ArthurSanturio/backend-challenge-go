BEGIN;
DROP TRIGGER pending_operation_job ON wager_transactions;
DROP FUNCTION schedule_pending_operation();
DROP TABLE reference_jobs;
DROP INDEX outbox_worker_due;
ALTER TABLE outbox_events
    DROP CONSTRAINT outbox_lease_pair,
    DROP CONSTRAINT outbox_delivery_destination,
    DROP COLUMN lease_token,
    DROP COLUMN lease_until,
    DROP COLUMN route_to_dlq,
    DROP COLUMN dead_lettered_at;
COMMIT;
