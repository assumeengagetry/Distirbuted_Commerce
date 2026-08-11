ALTER TABLE accounts
    ADD COLUMN balance_version BIGINT NOT NULL DEFAULT 0,
    ADD CONSTRAINT accounts_balance_version_valid CHECK (balance_version >= 0);
ALTER TABLE accounts
    ADD CONSTRAINT accounts_id_user_unique UNIQUE (id, user_id);
ALTER TABLE accounts
    ADD CONSTRAINT accounts_payment_identity_unique UNIQUE (id, user_id, currency);

ALTER TABLE orders
    ADD CONSTRAINT orders_id_user_unique UNIQUE (id, user_id);
ALTER TABLE orders
    ADD CONSTRAINT orders_payment_identity_unique UNIQUE (id, user_id, currency, total_amount);

CREATE TABLE idempotency_keys (
    actor_id UUID NOT NULL,
    operation VARCHAR(64) NOT NULL,
    key_hash BYTEA NOT NULL,
    request_hash BYTEA NOT NULL,
    resource_id UUID NOT NULL,
    state VARCHAR(16) NOT NULL,
    response_status SMALLINT,
    created_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    CONSTRAINT idempotency_keys_pk PRIMARY KEY (actor_id, operation, key_hash),
    CONSTRAINT idempotency_keys_operation_resource_unique UNIQUE (operation, resource_id),
    CONSTRAINT idempotency_keys_actor_fk FOREIGN KEY (actor_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT idempotency_keys_operation_valid CHECK (
        operation IN ('orders.create.v1', 'payments.create.v1')
    ),
    CONSTRAINT idempotency_keys_key_hash_length CHECK (octet_length(key_hash) = 32),
    CONSTRAINT idempotency_keys_request_hash_length CHECK (octet_length(request_hash) = 32),
    CONSTRAINT idempotency_keys_resource_non_nil CHECK (
        resource_id <> '00000000-0000-0000-0000-000000000000'::uuid
    ),
    CONSTRAINT idempotency_keys_state_valid CHECK (
        (
            state = 'reserved'
            AND response_status IS NULL
            AND completed_at IS NULL
        ) OR (
            state = 'completed'
            AND response_status BETWEEN 200 AND 299
            AND completed_at IS NOT NULL
            AND completed_at >= created_at
        )
    )
);

CREATE TABLE account_balance_entries (
    account_id UUID NOT NULL,
    user_id UUID NOT NULL,
    currency VARCHAR(3) NOT NULL,
    balance_version BIGINT NOT NULL,
    balance_before BIGINT NOT NULL,
    balance_after BIGINT NOT NULL,
    debit_amount BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT account_balance_entries_pk PRIMARY KEY (account_id, balance_version),
    CONSTRAINT account_balance_entries_payment_identity_unique UNIQUE (
        account_id, user_id, currency, balance_version,
        balance_before, balance_after, debit_amount
    ),
    CONSTRAINT account_balance_entries_account_fk FOREIGN KEY (account_id, user_id, currency)
        REFERENCES accounts (id, user_id, currency) ON DELETE CASCADE,
    CONSTRAINT account_balance_entries_currency_iso CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT account_balance_entries_version_positive CHECK (balance_version > 0),
    CONSTRAINT account_balance_entries_balances_non_negative CHECK (
        balance_before >= 0 AND balance_after >= 0
    ),
    CONSTRAINT account_balance_entries_debit_non_negative CHECK (debit_amount >= 0),
    CONSTRAINT account_balance_entries_debit_math CHECK (
        debit_amount::numeric = GREATEST(balance_before::numeric - balance_after::numeric, 0)
    )
);

CREATE FUNCTION version_account_balance() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.balance IS DISTINCT FROM OLD.balance THEN
        NEW.balance_version := OLD.balance_version + 1;
    ELSIF NEW.balance_version IS DISTINCT FROM OLD.balance_version THEN
        RAISE EXCEPTION 'account balance version cannot change without a balance change'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER accounts_version_balance
BEFORE UPDATE OF balance, balance_version ON accounts
FOR EACH ROW
EXECUTE FUNCTION version_account_balance();

CREATE FUNCTION record_account_balance_entry() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.balance IS DISTINCT FROM OLD.balance THEN
        INSERT INTO account_balance_entries (
            account_id, user_id, currency, balance_version,
            balance_before, balance_after, debit_amount, created_at
        ) VALUES (
            NEW.id, NEW.user_id, NEW.currency, NEW.balance_version,
            OLD.balance, NEW.balance, GREATEST(OLD.balance - NEW.balance, 0), clock_timestamp()
        );
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER accounts_record_balance_entry
AFTER UPDATE OF balance ON accounts
FOR EACH ROW
EXECUTE FUNCTION record_account_balance_entry();

CREATE TABLE payments (
    id UUID PRIMARY KEY,
    order_id UUID NOT NULL,
    user_id UUID NOT NULL,
    account_id UUID NOT NULL,
    account_balance_version BIGINT NOT NULL,
    status VARCHAR(20) NOT NULL,
    currency VARCHAR(3) NOT NULL,
    amount BIGINT NOT NULL,
    balance_before BIGINT NOT NULL,
    balance_after BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT payments_order_unique UNIQUE (order_id),
    CONSTRAINT payments_order_identity_fk FOREIGN KEY (order_id, user_id, currency, amount)
        REFERENCES orders (id, user_id, currency, total_amount) ON DELETE RESTRICT,
    CONSTRAINT payments_account_identity_fk FOREIGN KEY (account_id, user_id, currency)
        REFERENCES accounts (id, user_id, currency) ON DELETE RESTRICT,
    CONSTRAINT payments_balance_entry_fk FOREIGN KEY (
        account_id, user_id, currency, account_balance_version,
        balance_before, balance_after, amount
    ) REFERENCES account_balance_entries (
        account_id, user_id, currency, balance_version,
        balance_before, balance_after, debit_amount
    ) ON DELETE RESTRICT,
    CONSTRAINT payments_balance_entry_unique UNIQUE (account_id, account_balance_version),
    CONSTRAINT payments_status_valid CHECK (status = 'succeeded'),
    CONSTRAINT payments_currency_iso CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT payments_amount_positive CHECK (amount > 0),
    CONSTRAINT payments_balances_non_negative CHECK (balance_before >= 0 AND balance_after >= 0),
    CONSTRAINT payments_balance_sufficient CHECK (balance_before >= amount),
    CONSTRAINT payments_balance_math CHECK (
        balance_before::numeric - amount::numeric = balance_after::numeric
    )
);

CREATE FUNCTION enforce_payment_settlement() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    current_order_status VARCHAR(30);
    current_account_balance BIGINT;
BEGIN
    SELECT status INTO STRICT current_order_status
    FROM orders
    WHERE id = NEW.order_id AND user_id = NEW.user_id;

    SELECT balance INTO STRICT current_account_balance
    FROM accounts
    WHERE id = NEW.account_id AND user_id = NEW.user_id;

    IF current_order_status <> 'paid' OR current_account_balance <> NEW.balance_after THEN
        RAISE EXCEPTION 'payment settlement invariant violated'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE CONSTRAINT TRIGGER payments_settlement_consistent
AFTER INSERT ON payments
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW
EXECUTE FUNCTION enforce_payment_settlement();

CREATE FUNCTION enforce_payment_immutable() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'payments are immutable'
        USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER payments_immutable
BEFORE UPDATE ON payments
FOR EACH ROW
EXECUTE FUNCTION enforce_payment_immutable();

CREATE FUNCTION enforce_paid_order_terminal() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF OLD.status = 'paid' AND NEW.status <> 'paid' THEN
        RAISE EXCEPTION 'paid order status is terminal'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER orders_paid_status_terminal
BEFORE UPDATE OF status ON orders
FOR EACH ROW
EXECUTE FUNCTION enforce_paid_order_terminal();

CREATE INDEX idempotency_keys_created_idx ON idempotency_keys (created_at);
CREATE INDEX account_balance_entries_user_history_idx
    ON account_balance_entries (user_id, created_at DESC, account_id, balance_version DESC);
CREATE INDEX payments_user_history_idx ON payments (user_id, created_at DESC, id DESC);
CREATE INDEX payments_account_history_idx ON payments (account_id, created_at DESC, id DESC);
