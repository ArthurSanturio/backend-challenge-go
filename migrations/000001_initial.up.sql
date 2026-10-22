BEGIN;

CREATE FUNCTION financial_identifier(value TEXT) RETURNS BOOLEAN
LANGUAGE sql IMMUTABLE STRICT AS $$
    SELECT value <> '' AND value !~ '(^[[:space:]])|([[:space:]]$)'
$$;

CREATE TABLE wallets (
    id TEXT PRIMARY KEY CHECK (financial_identifier(id)),
    player_id TEXT NOT NULL CHECK (financial_identifier(player_id)),
    currency TEXT NOT NULL CHECK (currency IN ('BRL','USD','EUR')),
    balance_minor BIGINT NOT NULL CHECK (balance_minor >= 0),
    version BIGINT NOT NULL CHECK (version >= 1),
    created_at TIMESTAMPTZ NOT NULL CHECK (isfinite(created_at) AND created_at <> '0001-01-01T00:00:00Z'),
    updated_at TIMESTAMPTZ NOT NULL CHECK (isfinite(updated_at) AND updated_at >= created_at),
    UNIQUE (player_id, currency),
    UNIQUE (id, currency)
);

CREATE TABLE wager_transactions (
    id TEXT PRIMARY KEY CHECK (financial_identifier(id)),
    external_transaction_id TEXT,
    provider_id TEXT,
    idempotency_key TEXT,
    payload_hash TEXT,
    wallet_id TEXT NOT NULL REFERENCES wallets(id),
    player_id TEXT NOT NULL CHECK (financial_identifier(player_id)),
    round_id TEXT,
    game_id TEXT,
    kind TEXT NOT NULL CHECK (kind IN ('OPENING','BET','WIN','LOSS','REFUND','ROLLBACK')),
    amount_minor BIGINT NOT NULL CHECK (amount_minor >= 0),
    currency TEXT NOT NULL CHECK (currency IN ('BRL','USD','EUR')),
    reference_external_transaction_id TEXT CHECK (reference_external_transaction_id IS NULL OR financial_identifier(reference_external_transaction_id)),
    reference_transaction_id TEXT REFERENCES wager_transactions(id),
    status TEXT NOT NULL CHECK (status IN ('PENDING','PENDING_REFERENCE','PROCESSED','REJECTED','FAILED')),
    direction TEXT CHECK (direction IN ('DEBIT','CREDIT')),
    result_balance_minor BIGINT CHECK (result_balance_minor >= 0),
    failure_code TEXT,
    created_at TIMESTAMPTZ NOT NULL CHECK (isfinite(created_at) AND created_at <> '0001-01-01T00:00:00Z'),
    updated_at TIMESTAMPTZ NOT NULL CHECK (isfinite(updated_at) AND updated_at >= created_at),
    UNIQUE (provider_id, external_transaction_id),
    UNIQUE (provider_id, idempotency_key),
    UNIQUE (id, wallet_id, currency),
    CHECK ((kind = 'LOSS' AND amount_minor = 0) OR (kind <> 'LOSS' AND amount_minor > 0)),
    CHECK (
        (kind = 'OPENING' AND provider_id IS NULL AND external_transaction_id IS NULL
         AND idempotency_key IS NULL AND payload_hash IS NULL AND round_id IS NULL AND game_id IS NULL
         AND reference_external_transaction_id IS NULL AND status = 'PROCESSED')
        OR
        (kind <> 'OPENING' AND provider_id IS NOT NULL AND financial_identifier(provider_id)
         AND external_transaction_id IS NOT NULL AND financial_identifier(external_transaction_id)
         AND idempotency_key IS NOT NULL AND financial_identifier(idempotency_key)
         AND payload_hash IS NOT NULL AND payload_hash ~ '^[0-9a-f]{64}$'
         AND round_id IS NOT NULL AND financial_identifier(round_id)
         AND game_id IS NOT NULL AND financial_identifier(game_id))
    ),
    CHECK (
        (kind IN ('REFUND','ROLLBACK') AND reference_external_transaction_id IS NOT NULL)
        OR kind = 'WIN'
        OR (kind IN ('OPENING','BET','LOSS') AND reference_external_transaction_id IS NULL)
    ),
    CHECK (reference_external_transaction_id IS NULL OR reference_external_transaction_id <> external_transaction_id),
    CHECK (
        (status = 'PROCESSED' AND failure_code IS NULL AND result_balance_minor IS NOT NULL
         AND ((reference_external_transaction_id IS NULL AND reference_transaction_id IS NULL)
              OR (reference_external_transaction_id IS NOT NULL AND reference_transaction_id IS NOT NULL))
         AND ((kind IN ('OPENING','WIN','REFUND') AND COALESCE(direction,'') = 'CREDIT')
              OR (kind = 'BET' AND COALESCE(direction,'') = 'DEBIT')
              OR (kind = 'ROLLBACK' AND direction IS NOT NULL)
              OR (kind = 'LOSS' AND direction IS NULL)))
        OR
        (status <> 'PROCESSED' AND direction IS NULL AND result_balance_minor IS NULL AND reference_transaction_id IS NULL
         AND ((status IN ('PENDING','PENDING_REFERENCE') AND failure_code IS NULL)
              OR (status = 'FAILED' AND COALESCE(failure_code,'') = 'PERMANENT_INFRASTRUCTURE_FAILURE')
              OR (status = 'REJECTED' AND (
                  (kind = 'BET' AND COALESCE(failure_code,'') = 'INSUFFICIENT_FUNDS')
                  OR (kind = 'ROLLBACK' AND COALESCE(failure_code,'') = 'REVERSAL_INSUFFICIENT_FUNDS')
                  OR (reference_external_transaction_id IS NOT NULL AND COALESCE(failure_code,'') IN ('REFERENCE_NOT_FOUND','REFERENCE_NOT_PROCESSED','REFERENCE_MISMATCH'))
                  OR (kind IN ('REFUND','ROLLBACK') AND COALESCE(failure_code,'') = 'ALREADY_REVERSED')
                  OR (kind <> 'OPENING' AND COALESCE(failure_code,'') = 'INVALID_OPERATION')
              ))))
    ),
    CHECK (status <> 'PENDING_REFERENCE' OR reference_external_transaction_id IS NOT NULL)
);

CREATE UNIQUE INDEX wager_one_opening ON wager_transactions(wallet_id) WHERE kind = 'OPENING';
-- Também impede combinar REFUND e ROLLBACK diretamente sobre a mesma BET.
CREATE UNIQUE INDEX wager_one_processed_reversal ON wager_transactions(reference_transaction_id)
    WHERE status = 'PROCESSED' AND kind IN ('REFUND','ROLLBACK');
CREATE INDEX wager_pending ON wager_transactions(status,created_at,id) WHERE status IN ('PENDING','PENDING_REFERENCE');

CREATE TABLE wallet_ledger_entries (
    id TEXT PRIMARY KEY CHECK (financial_identifier(id)),
    wallet_id TEXT NOT NULL,
    transaction_id TEXT NOT NULL,
    direction TEXT NOT NULL CHECK (direction IN ('DEBIT','CREDIT')),
    amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
    currency TEXT NOT NULL,
    balance_before_minor BIGINT NOT NULL CHECK (balance_before_minor >= 0),
    balance_after_minor BIGINT NOT NULL CHECK (balance_after_minor >= 0),
    wallet_version BIGINT NOT NULL CHECK (wallet_version >= 1),
    created_at TIMESTAMPTZ NOT NULL CHECK (isfinite(created_at) AND created_at <> '0001-01-01T00:00:00Z'),
    FOREIGN KEY (wallet_id,currency) REFERENCES wallets(id,currency),
    FOREIGN KEY (transaction_id,wallet_id,currency) REFERENCES wager_transactions(id,wallet_id,currency),
    UNIQUE (wallet_id,transaction_id),
    UNIQUE (wallet_id,wallet_version),
    CHECK (balance_after_minor::NUMERIC = balance_before_minor::NUMERIC
        + CASE WHEN direction = 'CREDIT' THEN amount_minor::NUMERIC ELSE -amount_minor::NUMERIC END)
);
CREATE INDEX ledger_page ON wallet_ledger_entries(wallet_id,created_at,id);

CREATE TABLE outbox_events (
    event_id TEXT PRIMARY KEY CHECK (financial_identifier(event_id)),
    aggregate_id TEXT NOT NULL CHECK (financial_identifier(aggregate_id)),
    event_type TEXT NOT NULL CHECK (event_type IN (
        'WagerTransactionProcessed','WagerTransactionRejected','WagerTransactionPendingReference','WalletBalanceChanged')),
    version INTEGER NOT NULL CHECK (version = 1),
    occurred_at TIMESTAMPTZ NOT NULL CHECK (isfinite(occurred_at) AND occurred_at <> '0001-01-01T00:00:00Z'),
    transaction_id TEXT NOT NULL REFERENCES wager_transactions(id),
    payload TEXT NOT NULL CHECK (jsonb_typeof(payload::JSONB) = 'object'),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    published_at TIMESTAMPTZ,
    UNIQUE (transaction_id,event_type)
);
CREATE INDEX outbox_pending ON outbox_events(next_attempt_at,event_id) WHERE published_at IS NULL;

CREATE FUNCTION deny_financial_mutation() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'financial audit records cannot be edited, deleted or truncated'
        USING ERRCODE = '23514';
END
$$;

CREATE TRIGGER ledger_immutable BEFORE UPDATE OR DELETE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION deny_financial_mutation();
CREATE TRIGGER ledger_no_truncate BEFORE TRUNCATE ON wallet_ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION deny_financial_mutation();
CREATE TRIGGER wallets_no_delete BEFORE DELETE ON wallets
    FOR EACH ROW EXECUTE FUNCTION deny_financial_mutation();
CREATE TRIGGER wallets_no_truncate BEFORE TRUNCATE ON wallets
    FOR EACH STATEMENT EXECUTE FUNCTION deny_financial_mutation();
CREATE TRIGGER wagers_no_truncate BEFORE TRUNCATE ON wager_transactions
    FOR EACH STATEMENT EXECUTE FUNCTION deny_financial_mutation();
CREATE TRIGGER outbox_no_delete BEFORE DELETE ON outbox_events
    FOR EACH ROW EXECUTE FUNCTION deny_financial_mutation();
CREATE TRIGGER outbox_no_truncate BEFORE TRUNCATE ON outbox_events
    FOR EACH STATEMENT EXECUTE FUNCTION deny_financial_mutation();

CREATE FUNCTION guard_wallet_state() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.version <> 1 OR NEW.updated_at <> NEW.created_at THEN
            RAISE EXCEPTION 'wallet must start at version 1' USING ERRCODE = '23514';
        END IF;
    ELSE
        IF ROW(NEW.id,NEW.player_id,NEW.currency,NEW.created_at)
            IS DISTINCT FROM ROW(OLD.id,OLD.player_id,OLD.currency,OLD.created_at)
            OR NEW.updated_at < OLD.updated_at THEN
            RAISE EXCEPTION 'wallet identity or timestamps cannot be changed' USING ERRCODE = '23514';
        END IF;
        IF (NEW.balance_minor <> OLD.balance_minor AND NEW.version::NUMERIC <> OLD.version::NUMERIC + 1)
            OR (NEW.balance_minor = OLD.balance_minor AND NEW.version <> OLD.version) THEN
            RAISE EXCEPTION 'wallet version must track balance changes' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER wallet_state BEFORE INSERT OR UPDATE ON wallets
    FOR EACH ROW EXECUTE FUNCTION guard_wallet_state();

CREATE FUNCTION guard_wager_state() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    wallet wallets%ROWTYPE;
    original wager_transactions%ROWTYPE;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'transactions cannot be deleted' USING ERRCODE = '23514';
    END IF;
    IF TG_OP = 'UPDATE' THEN
        IF OLD.status IN ('PROCESSED','REJECTED','FAILED') THEN
            RAISE EXCEPTION 'terminal transaction is immutable' USING ERRCODE = '23514';
        END IF;
        IF ROW(NEW.id,NEW.external_transaction_id,NEW.provider_id,NEW.idempotency_key,NEW.payload_hash,
                NEW.wallet_id,NEW.player_id,NEW.round_id,NEW.game_id,NEW.kind,NEW.amount_minor,NEW.currency,
                NEW.reference_external_transaction_id,NEW.created_at)
            IS DISTINCT FROM ROW(OLD.id,OLD.external_transaction_id,OLD.provider_id,OLD.idempotency_key,OLD.payload_hash,
                OLD.wallet_id,OLD.player_id,OLD.round_id,OLD.game_id,OLD.kind,OLD.amount_minor,OLD.currency,
                OLD.reference_external_transaction_id,OLD.created_at)
            OR NEW.updated_at < OLD.updated_at OR NEW.status = OLD.status
            OR (OLD.status = 'PENDING_REFERENCE' AND NEW.status = 'PENDING') THEN
            RAISE EXCEPTION 'invalid transaction transition or input mutation' USING ERRCODE = '23514';
        END IF;
    END IF;
    IF NEW.status = 'PROCESSED' THEN
        SELECT * INTO wallet FROM wallets WHERE id = NEW.wallet_id;
        IF NOT FOUND OR wallet.player_id <> NEW.player_id OR wallet.currency <> NEW.currency THEN
            RAISE EXCEPTION 'processed transaction does not match wallet' USING ERRCODE = '23514';
        END IF;
        IF NEW.kind = 'OPENING' AND NEW.result_balance_minor <> NEW.amount_minor THEN
            RAISE EXCEPTION 'invalid opening result' USING ERRCODE = '23514';
        END IF;
        IF NEW.reference_transaction_id IS NOT NULL THEN
            SELECT * INTO original FROM wager_transactions WHERE id = NEW.reference_transaction_id;
            IF NOT FOUND OR original.status <> 'PROCESSED' OR original.id = NEW.id
                OR ROW(original.provider_id,original.external_transaction_id,original.player_id,original.wallet_id,original.currency,original.round_id)
                    IS DISTINCT FROM ROW(NEW.provider_id,NEW.reference_external_transaction_id,NEW.player_id,NEW.wallet_id,NEW.currency,NEW.round_id)
                OR NEW.updated_at < original.updated_at THEN
                RAISE EXCEPTION 'invalid processed reference' USING ERRCODE = '23514';
            END IF;
            IF (NEW.kind IN ('WIN','REFUND') AND original.kind <> 'BET')
                OR (NEW.kind = 'ROLLBACK' AND original.kind NOT IN ('BET','WIN','REFUND'))
                OR (NEW.kind IN ('REFUND','ROLLBACK') AND NEW.amount_minor <> original.amount_minor)
                OR (NEW.kind = 'ROLLBACK' AND NEW.direction <> CASE WHEN original.kind = 'BET' THEN 'CREDIT' ELSE 'DEBIT' END) THEN
                RAISE EXCEPTION 'invalid reversal kind, direction or amount' USING ERRCODE = '23514';
            END IF;
        END IF;
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER wager_state BEFORE INSERT OR UPDATE OR DELETE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION guard_wager_state();

CREATE FUNCTION guard_ledger_insert() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    operation wager_transactions%ROWTYPE;
    wallet wallets%ROWTYPE;
    previous wallet_ledger_entries%ROWTYPE;
BEGIN
    SELECT * INTO wallet FROM wallets WHERE id = NEW.wallet_id FOR UPDATE;
    IF NOT FOUND OR NEW.wallet_version <> wallet.version OR NEW.balance_after_minor <> wallet.balance_minor THEN
        RAISE EXCEPTION 'ledger does not match current wallet version and balance' USING ERRCODE = '23514';
    END IF;
    SELECT * INTO operation FROM wager_transactions WHERE id = NEW.transaction_id;
    IF NOT FOUND OR operation.status <> 'PROCESSED' OR operation.kind = 'LOSS'
        OR ROW(NEW.wallet_id,NEW.direction,NEW.amount_minor,NEW.currency,NEW.balance_after_minor)
            IS DISTINCT FROM ROW(operation.wallet_id,operation.direction,operation.amount_minor,operation.currency,operation.result_balance_minor)
        OR NEW.created_at < operation.updated_at THEN
        RAISE EXCEPTION 'ledger does not match processed transaction' USING ERRCODE = '23514';
    END IF;
    SELECT * INTO previous FROM wallet_ledger_entries WHERE wallet_id = NEW.wallet_id ORDER BY wallet_version DESC LIMIT 1;
    IF FOUND THEN
        IF NEW.wallet_version::NUMERIC <> previous.wallet_version::NUMERIC + 1 OR NEW.balance_before_minor <> previous.balance_after_minor
            OR operation.kind = 'OPENING' THEN
            RAISE EXCEPTION 'ledger chain is not continuous' USING ERRCODE = '23514';
        END IF;
    ELSE
        IF NEW.balance_before_minor <> 0 OR
            (operation.kind = 'OPENING' AND NEW.wallet_version <> 1) OR
            (operation.kind <> 'OPENING' AND NEW.wallet_version <> 2) THEN
            RAISE EXCEPTION 'invalid first ledger entry' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER ledger_insert BEFORE INSERT ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION guard_ledger_insert();

CREATE FUNCTION check_wallet_financial_state() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    wallet wallets%ROWTYPE;
    last_entry wallet_ledger_entries%ROWTYPE;
BEGIN
    -- A abertura deve preservar sua própria origem, mesmo se outras operações
    -- forem realizadas na mesma transação SQL antes do commit.
    IF TG_OP = 'INSERT' AND NEW.balance_minor > 0 AND NOT EXISTS (
        SELECT 1 FROM wallet_ledger_entries entry
        JOIN wager_transactions operation ON operation.id = entry.transaction_id
        WHERE entry.wallet_id = NEW.id AND entry.wallet_version = 1
            AND operation.kind = 'OPENING' AND entry.amount_minor = NEW.balance_minor
    ) THEN
        RAISE EXCEPTION 'initial positive balance requires its opening operation' USING ERRCODE = '23514';
    END IF;
    SELECT * INTO wallet FROM wallets WHERE id = NEW.id;
    SELECT * INTO last_entry FROM wallet_ledger_entries WHERE wallet_id = NEW.id ORDER BY wallet_version DESC LIMIT 1;
    IF FOUND THEN
        IF wallet.balance_minor <> last_entry.balance_after_minor OR wallet.version <> last_entry.wallet_version THEN
            RAISE EXCEPTION 'wallet balance change requires matching ledger entry' USING ERRCODE = '23514';
        END IF;
    ELSIF wallet.balance_minor <> 0 OR wallet.version <> 1 THEN
        RAISE EXCEPTION 'initial positive balance requires opening ledger' USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER wallet_financial_state AFTER INSERT OR UPDATE ON wallets
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_wallet_financial_state();

CREATE FUNCTION financial_decimal(units BIGINT) RETURNS TEXT LANGUAGE sql IMMUTABLE STRICT AS $$
    SELECT (units / 100)::TEXT || '.' || lpad((units % 100)::TEXT,2,'0')
$$;

CREATE FUNCTION guard_outbox_snapshot() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    envelope JSONB;
    data JSONB;
    operation wager_transactions%ROWTYPE;
    entry wallet_ledger_entries%ROWTYPE;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF ROW(NEW.event_id,NEW.aggregate_id,NEW.event_type,NEW.version,NEW.occurred_at,NEW.transaction_id,NEW.payload)
            IS DISTINCT FROM ROW(OLD.event_id,OLD.aggregate_id,OLD.event_type,OLD.version,OLD.occurred_at,OLD.transaction_id,OLD.payload) THEN
            RAISE EXCEPTION 'outbox event snapshot is immutable' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    envelope := NEW.payload::JSONB;
    data := envelope -> 'data';
    SELECT * INTO operation FROM wager_transactions WHERE id = NEW.transaction_id;
    IF NOT FOUND OR jsonb_typeof(data) IS DISTINCT FROM 'object'
        OR envelope ->> 'eventId' IS DISTINCT FROM NEW.event_id
        OR envelope ->> 'eventType' IS DISTINCT FROM NEW.event_type
        OR envelope ->> 'aggregateId' IS DISTINCT FROM NEW.aggregate_id
        OR jsonb_typeof(envelope -> 'version') IS DISTINCT FROM 'number'
        OR (envelope ->> 'version')::INTEGER IS DISTINCT FROM NEW.version
        OR NOT COALESCE(financial_identifier(envelope ->> 'correlationId'),FALSE)
        OR (envelope ->> 'occurredAt')::TIMESTAMPTZ IS DISTINCT FROM NEW.occurred_at
        OR NEW.occurred_at <> operation.updated_at
        OR data ->> 'transactionId' IS DISTINCT FROM operation.id
        OR data ->> 'walletId' IS DISTINCT FROM operation.wallet_id
        OR data #>> '{money,amount}' IS DISTINCT FROM financial_decimal(operation.amount_minor)
        OR data #>> '{money,currency}' IS DISTINCT FROM operation.currency
        OR jsonb_typeof(data #> '{money,amount}') IS DISTINCT FROM 'string' THEN
        RAISE EXCEPTION 'outbox envelope does not match its operation' USING ERRCODE = '23514';
    END IF;
    IF NEW.event_type = 'WalletBalanceChanged' THEN
        SELECT * INTO entry FROM wallet_ledger_entries WHERE transaction_id = operation.id AND wallet_id = operation.wallet_id;
        IF NOT FOUND OR NEW.aggregate_id <> operation.wallet_id
            OR data ->> 'direction' IS DISTINCT FROM entry.direction
            OR data #>> '{balanceBefore,amount}' IS DISTINCT FROM financial_decimal(entry.balance_before_minor)
            OR data #>> '{balanceBefore,currency}' IS DISTINCT FROM entry.currency
            OR jsonb_typeof(data #> '{balanceBefore,amount}') IS DISTINCT FROM 'string'
            OR data #>> '{balanceAfter,amount}' IS DISTINCT FROM financial_decimal(entry.balance_after_minor)
            OR data #>> '{balanceAfter,currency}' IS DISTINCT FROM entry.currency
            OR jsonb_typeof(data #> '{balanceAfter,amount}') IS DISTINCT FROM 'string'
            OR (data ->> 'walletVersion')::BIGINT IS DISTINCT FROM entry.wallet_version THEN
            RAISE EXCEPTION 'balance event does not match ledger' USING ERRCODE = '23514';
        END IF;
    ELSE
        IF NEW.aggregate_id <> operation.id OR data ->> 'kind' IS DISTINCT FROM operation.kind
            OR data ->> 'playerId' IS DISTINCT FROM operation.player_id
            OR data ->> 'providerId' IS DISTINCT FROM operation.provider_id
            OR data ->> 'externalTransactionId' IS DISTINCT FROM operation.external_transaction_id
            OR data ->> 'roundId' IS DISTINCT FROM operation.round_id
            OR data ->> 'gameId' IS DISTINCT FROM operation.game_id
            OR data ->> 'referenceExternalTransactionId' IS DISTINCT FROM operation.reference_external_transaction_id THEN
            RAISE EXCEPTION 'transaction event data does not match operation' USING ERRCODE = '23514';
        END IF;
        IF (NEW.event_type = 'WagerTransactionProcessed' AND
                (operation.status <> 'PROCESSED'
                 OR data #>> '{balance,amount}' IS DISTINCT FROM financial_decimal(operation.result_balance_minor)
                 OR jsonb_typeof(data #> '{balance,amount}') IS DISTINCT FROM 'string'
                 OR data #>> '{balance,currency}' IS DISTINCT FROM operation.currency))
            OR (NEW.event_type = 'WagerTransactionRejected' AND
                (operation.status <> 'REJECTED' OR data ->> 'failureCode' IS DISTINCT FROM operation.failure_code))
            OR (NEW.event_type = 'WagerTransactionPendingReference' AND operation.status <> 'PENDING_REFERENCE') THEN
            RAISE EXCEPTION 'event does not match transaction state' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER outbox_snapshot BEFORE INSERT OR UPDATE ON outbox_events
    FOR EACH ROW EXECUTE FUNCTION guard_outbox_snapshot();

CREATE FUNCTION check_wager_financial_records() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    operation wager_transactions%ROWTYPE;
    required_event TEXT;
BEGIN
    SELECT * INTO operation FROM wager_transactions WHERE id = NEW.id;
    CASE operation.status
        WHEN 'PROCESSED' THEN required_event := 'WagerTransactionProcessed';
        WHEN 'REJECTED' THEN required_event := 'WagerTransactionRejected';
        WHEN 'PENDING_REFERENCE' THEN required_event := 'WagerTransactionPendingReference';
        ELSE RETURN NULL;
    END CASE;
    IF NOT EXISTS (SELECT 1 FROM outbox_events WHERE transaction_id = operation.id AND event_type = required_event) THEN
        RAISE EXCEPTION 'transaction requires its durable event' USING ERRCODE = '23514';
    END IF;
    IF operation.status = 'PROCESSED' AND operation.kind <> 'LOSS' THEN
        IF NOT EXISTS (SELECT 1 FROM wallet_ledger_entries WHERE transaction_id = operation.id AND wallet_id = operation.wallet_id)
            OR NOT EXISTS (SELECT 1 FROM outbox_events WHERE transaction_id = operation.id AND event_type = 'WalletBalanceChanged') THEN
            RAISE EXCEPTION 'processed movement requires ledger and balance event' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER wager_financial_records AFTER INSERT OR UPDATE ON wager_transactions
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_wager_financial_records();

COMMIT;
