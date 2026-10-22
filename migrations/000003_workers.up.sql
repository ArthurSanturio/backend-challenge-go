BEGIN;

ALTER TABLE outbox_events
    ADD COLUMN lease_token TEXT,
    ADD COLUMN lease_until TIMESTAMPTZ,
    ADD COLUMN route_to_dlq BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN dead_lettered_at TIMESTAMPTZ,
    ADD CONSTRAINT outbox_lease_pair CHECK ((lease_token IS NULL) = (lease_until IS NULL)),
    ADD CONSTRAINT outbox_delivery_destination CHECK (published_at IS NULL OR dead_lettered_at IS NULL);
CREATE INDEX outbox_worker_due ON outbox_events(next_attempt_at,event_id)
    WHERE published_at IS NULL AND dead_lettered_at IS NULL;

CREATE TABLE reference_jobs (
    transaction_id TEXT PRIMARY KEY REFERENCES wager_transactions(id),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    lease_token TEXT,
    lease_until TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    CHECK ((lease_token IS NULL) = (lease_until IS NULL))
);
CREATE INDEX reference_job_due ON reference_jobs(next_attempt_at,transaction_id) WHERE completed_at IS NULL;

CREATE FUNCTION schedule_pending_operation() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status IN ('PENDING','PENDING_REFERENCE') THEN
        INSERT INTO reference_jobs(transaction_id) VALUES (NEW.id) ON CONFLICT DO NOTHING;
    END IF;
    RETURN NULL;
END
$$;
CREATE TRIGGER pending_operation_job AFTER INSERT OR UPDATE OF status ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION schedule_pending_operation();
INSERT INTO reference_jobs(transaction_id)
    SELECT id FROM wager_transactions WHERE status IN ('PENDING','PENDING_REFERENCE');

COMMIT;
