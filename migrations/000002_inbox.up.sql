BEGIN;

CREATE TABLE inbox_messages (
    consumer_name TEXT NOT NULL CHECK (financial_identifier(consumer_name)),
    message_id TEXT NOT NULL CHECK (financial_identifier(message_id)),
    provider_id TEXT NOT NULL CHECK (financial_identifier(provider_id)),
    payload_hash TEXT NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    received_at TIMESTAMPTZ NOT NULL CHECK (isfinite(received_at)),
    transaction_id TEXT REFERENCES wager_transactions(id),
    completed_at TIMESTAMPTZ CHECK (isfinite(completed_at) AND completed_at >= received_at),
    PRIMARY KEY (consumer_name,message_id),
    CHECK ((transaction_id IS NULL) = (completed_at IS NULL))
);

CREATE FUNCTION guard_inbox_completion() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.completed_at IS NOT NULL OR
        ROW(NEW.consumer_name,NEW.message_id,NEW.provider_id,NEW.payload_hash,NEW.received_at)
        IS DISTINCT FROM ROW(OLD.consumer_name,OLD.message_id,OLD.provider_id,OLD.payload_hash,OLD.received_at) THEN
        RAISE EXCEPTION 'inbox identity and completed receipts are immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER inbox_immutable BEFORE UPDATE ON inbox_messages
    FOR EACH ROW EXECUTE FUNCTION guard_inbox_completion();
CREATE TRIGGER inbox_no_delete BEFORE DELETE ON inbox_messages
    FOR EACH ROW EXECUTE FUNCTION deny_financial_mutation();
CREATE TRIGGER inbox_no_truncate BEFORE TRUNCATE ON inbox_messages
    FOR EACH STATEMENT EXECUTE FUNCTION deny_financial_mutation();

CREATE FUNCTION check_inbox_completed() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    receipt inbox_messages%ROWTYPE;
BEGIN
    SELECT * INTO receipt FROM inbox_messages
        WHERE consumer_name=NEW.consumer_name AND message_id=NEW.message_id;
    IF receipt.completed_at IS NULL OR NOT EXISTS (
        SELECT 1 FROM wager_transactions WHERE id=receipt.transaction_id
            AND provider_id=receipt.provider_id AND status IN ('PROCESSED','REJECTED','PENDING_REFERENCE','FAILED')
    ) THEN
        RAISE EXCEPTION 'inbox requires a durable treatment in the same commit' USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER inbox_completed AFTER INSERT OR UPDATE ON inbox_messages
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_inbox_completed();

COMMIT;
