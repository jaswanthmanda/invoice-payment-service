-- IDs are UUIDv7, generated in application code (google/uuid NewV7).
-- Money is BIGINT cents. Timestamps are TIMESTAMPTZ.

CREATE TABLE businesses (
    id         UUID PRIMARY KEY,
    name       TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
    id           UUID PRIMARY KEY,
    business_id  UUID NOT NULL REFERENCES businesses(id),
    prefix       TEXT NOT NULL UNIQUE,
    key_hash     TEXT NOT NULL,
    revoked_at   TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX api_keys_business_idx ON api_keys (business_id);

CREATE TABLE customers (
    id          UUID PRIMARY KEY,
    business_id UUID NOT NULL REFERENCES businesses(id),
    name        TEXT NOT NULL,
    email       TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (business_id, email)
);
CREATE INDEX customers_list_idx ON customers (business_id, id DESC);

CREATE TABLE invoices (
    id          UUID PRIMARY KEY,
    business_id UUID NOT NULL REFERENCES businesses(id),
    customer_id UUID NOT NULL REFERENCES customers(id),
    status      TEXT NOT NULL DEFAULT 'open'
                CHECK (status IN ('open', 'paid', 'void')),
    total_cents BIGINT NOT NULL CHECK (total_cents >= 0),
    currency    CHAR(3) NOT NULL DEFAULT 'USD' CHECK (currency = 'USD'),
    due_date    DATE NOT NULL,
    paid_at     TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX invoices_list_idx ON invoices (business_id, status, id DESC);
CREATE INDEX invoices_customer_idx ON invoices (customer_id);

CREATE TABLE invoice_line_items (
    id                UUID PRIMARY KEY,
    invoice_id        UUID NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
    description       TEXT NOT NULL CHECK (length(description) BETWEEN 1 AND 500),
    quantity          INT NOT NULL CHECK (quantity > 0),
    unit_amount_cents BIGINT NOT NULL CHECK (unit_amount_cents >= 0)
);
CREATE INDEX line_items_invoice_idx ON invoice_line_items (invoice_id);

CREATE TABLE payment_attempts (
    id           UUID PRIMARY KEY,
    invoice_id   UUID NOT NULL REFERENCES invoices(id),
    business_id  UUID NOT NULL REFERENCES businesses(id),
    amount_cents BIGINT NOT NULL CHECK (amount_cents > 0),
    card_token   TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending', 'succeeded', 'failed', 'unknown')),
    psp_ref      TEXT UNIQUE,
    failure_code TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ
);
-- Concurrency backstop: at most one unresolved attempt per invoice.
CREATE UNIQUE INDEX payment_attempts_one_inflight
    ON payment_attempts (invoice_id)
    WHERE status IN ('pending', 'unknown');
CREATE INDEX payment_attempts_invoice_idx ON payment_attempts (invoice_id, created_at);

CREATE TABLE idempotency_keys (
    business_id        UUID NOT NULL REFERENCES businesses(id),
    key                TEXT NOT NULL CHECK (length(key) BETWEEN 1 AND 255),
    request_hash       TEXT NOT NULL,
    payment_attempt_id UUID REFERENCES payment_attempts(id),
    response_status    INT,
    response_body      JSONB,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at         TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (business_id, key)
);
CREATE INDEX idempotency_expires_idx ON idempotency_keys (expires_at);

CREATE TABLE webhook_endpoints (
    id          UUID PRIMARY KEY,
    business_id UUID NOT NULL REFERENCES businesses(id),
    url         TEXT NOT NULL,
    secret      TEXT NOT NULL,
    enabled     BOOLEAN NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX webhook_endpoints_active_idx ON webhook_endpoints (business_id) WHERE enabled;

CREATE TABLE webhook_events (
    id          UUID PRIMARY KEY,
    business_id UUID NOT NULL REFERENCES businesses(id),
    invoice_id  UUID NOT NULL REFERENCES invoices(id),
    type        TEXT NOT NULL
                CHECK (type IN ('invoice.created', 'invoice.paid', 'invoice.payment_failed')),
    payload     JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX webhook_events_list_idx ON webhook_events (business_id, id DESC);
CREATE INDEX webhook_events_invoice_idx ON webhook_events (invoice_id);

CREATE TABLE webhook_deliveries (
    id               UUID PRIMARY KEY,
    event_id         UUID NOT NULL REFERENCES webhook_events(id),
    endpoint_id      UUID NOT NULL REFERENCES webhook_endpoints(id),
    status           TEXT NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending', 'delivered', 'failed')),
    attempt_count    INT NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at  TIMESTAMPTZ,
    last_status_code INT,
    last_error       TEXT,
    delivered_at     TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (event_id, endpoint_id)
);
CREATE INDEX webhook_deliveries_due_idx
    ON webhook_deliveries (next_attempt_at) WHERE status = 'pending';
CREATE INDEX webhook_deliveries_endpoint_idx ON webhook_deliveries (endpoint_id);