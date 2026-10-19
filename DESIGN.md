# Design

## Overview

```
 client ──► api ──► PostgreSQL ◄── worker ──► business webhook URLs
             │                        │
             └────► mock PSP ◄────────┘
        (api charges, worker looks up unresolved charges)
```

- **api**: stateless HTTP. It never calls a webhook URL.
- **worker**: separate process running the webhook dispatcher and the payment reconciler.
- **psp**: mock processor, treated as an external dependency with a hard 5 s client timeout.
- **PostgreSQL** is the only state: business data, idempotency keys and the webhook outbox. No Redis or broker (see section 6).

## 1. Data model

Conventions: UUIDv7 primary keys generated in the app (time-ordered, not guessable, and `id` doubles as the keyset pagination cursor); money is `BIGINT` cents; `TIMESTAMPTZ` everywhere; `TEXT` + `CHECK` instead of Postgres enums, so adding a state is a one-line migration; every tenant table carries `business_id`, and every query filters on it.

```
business ─┬─< api_keys
          ├─< customers ─< invoices ─┬─< invoice_line_items
          │                          └─< payment_attempts
          ├─< idempotency_keys
          ├─< webhook_endpoints ─< webhook_deliveries >─ webhook_events
          └─< webhook_events
```

| Table | Shape | Indexes and constraints |
|---|---|---|
| businesses | id, name | |
| api_keys | business_id, prefix, key_hash, revoked_at, last_used_at | unique `prefix` |
| customers | business_id, name, email | unique (business_id, email); (business_id, id DESC) for lists |
| invoices | business_id, customer_id, status, total_cents, currency, due_date, paid_at | CHECK status in (open, paid, void), total ≥ 0, currency = USD; (business_id, status, id DESC) |
| invoice_line_items | invoice_id, description, quantity, unit_amount_cents | CHECK quantity > 0, amount ≥ 0 |
| payment_attempts | invoice_id, business_id, amount_cents, card_token, status, psp_ref, failure_code | **partial unique (invoice_id) WHERE status IN (pending, unknown)**; unique psp_ref |
| idempotency_keys | PK (business_id, key), request_hash, payment_attempt_id, response_status, response_body, expires_at | index on expires_at |
| webhook_endpoints | business_id, url, secret, enabled | partial (business_id) WHERE enabled |
| webhook_events | business_id, invoice_id, type, payload | (business_id, id DESC) |
| webhook_deliveries | event_id, endpoint_id, status, attempt_count, next_attempt_at, last_status_code, last_error | unique (event_id, endpoint_id); partial (next_attempt_at) WHERE status = pending |

**Why this shape**
- `total_cents` is stored and computed once by the server at creation. Deriving it with `SUM` on every read would let it drift from what was charged.
- Line items are a table, not JSONB, so quantities and amounts stay typed and constrained.
- Invoice status and attempt status are separate. A failed card is an attempt outcome, not an invoice state, so retries stay possible.
- Events and deliveries are split: one event fans out to N endpoints, each with its own retry state.
- Idempotency lives in Postgres, not Redis, so the key and the attempt commit in the same transaction.
- Rules are enforced in the database (CHECKs, the partial unique index), not only in handlers.

**At 100x:** partition `invoices`, `payment_attempts` and `webhook_*` by month and archive old partitions; read replicas for list endpoints; PgBouncer; a TTL store for idempotency keys; move the outbox to Kafka or SQS while keeping the table as source of truth; shard by `business_id` only if one primary saturates.

## 2. Invoice state machine

```
 (create) ──► open ──PSP success──► paid   (terminal)
                │
                └────void request──► void   (terminal)

 open ──PSP failure──► open   (attempt marked failed, invoice unchanged)
```

| From | To | Trigger | Event |
|---|---|---|---|
| none | open | `POST /invoices` | `invoice.created` |
| open | paid | PSP confirms the charge | `invoice.paid` |
| open | void | `POST /invoices/{id}/void` | none |
| open | open | PSP declines | `invoice.payment_failed` |

- **Terminal:** `paid` and `void`. **Reversible:** nothing. Refunds would be a separate concept, not a reverse transition.
- **Rejection:** all rules live in one function, `CanTransition`. Every transition is also a conditional write (`UPDATE ... WHERE status = 'open'`). A transition that cannot apply returns `409 invalid_state_transition` with the current status, and a `CHECK` constraint is the last backstop.
- **Deliberate cuts:** no `draft` (the brief has no finalize step) and no `uncollectible` (it needs overdue detection and dunning).

## 3. Payment correctness and failure modes

**Mechanism:** a short row lock on the invoice (`SELECT ... FOR UPDATE`), the partial unique index on `payment_attempts`, and status-conditional updates. No lock or transaction is ever held across the PSP call.

1. **Tx 1 (short):** claim the idempotency key, lock the invoice, check it is payable, insert a `pending` attempt, commit.
2. **PSP call:** no transaction, 5 s timeout. The attempt ID is the PSP reference, and the PSP dedupes on it.
3. **Tx 2 (short):** record the outcome, update the invoice with a conditional `UPDATE`, insert the webhook event, store the response under the idempotency key.

**Why not the alternatives:** holding the lock during the PSP call lets a 30 s hang pin a connection and block the invoice. Optimistic concurrency finds out about the conflict after the card was already charged. Serializable isolation needs retry loops for one hot row. Advisory locks add a second mechanism where the row lock already works.

**(a) Two simultaneous pays.** The row lock orders them. The first inserts a `pending` attempt and commits. The invoice is still `open` until the PSP answers, so the others pass the status check but their insert hits the partial unique index and returns `409 payment_in_progress`. The lock gives the order, and the index guarantees at most one unresolved attempt even if application code had a bug. Test: 10 concurrent requests gave one 200, nine 409s and one PSP charge.

**(b) PSP timeout.** The client gives up at 5 s and the endpoint returns `202`. The attempt is `unknown`, the invoice stays `open`, and no payment-result webhook is sent yet. The caller finds out by polling `GET /invoices/{id}`, retrying with the same key, or receiving the webhook once resolved. The reconciler (every 15 s) asks the PSP about the attempt using the same reference and applies the result in one transaction. While the attempt is unresolved, pay and void both return `409`, because another payment could double-charge and a void could race a successful charge.

**(c) PSP succeeds, the service crashes before saving.** The attempt stays `pending` and the invoice `open`. A retry with the same key reports the current state (`202`) and does not start a new charge. After 60 s the reconciler looks the charge up by the same reference, finds it succeeded, and marks the invoice paid. The customer is charged once, because the PSP dedupes on the reference and new attempts are blocked while one is unresolved. If the PSP has no record of the reference, the reconciler waits 5 minutes (a delayed request could still arrive), then marks the attempt `failed` / `psp_no_record` and the invoice is payable again.

**(d) Idempotency key reused with a different body.** The request hash (method, path, invoice, card token) differs, so the API returns `422 idempotency_key_reuse` with no PSP call. Same key and same request replays the stored response byte for byte with `Idempotent-Replayed: true`. Keys are scoped per business, kept 24 h, and claimed with `INSERT ... ON CONFLICT DO NOTHING` on the primary key. Early rejections (404, 409) roll the key back, so a key exists only if an attempt was created.

**(e) Pay an invoice that is already paid.** A new key gets `409 invoice_not_payable` (no attempt, no PSP call). The original key replays the original `200`.

**Trade-off:** while an attempt is `unknown`, the customer cannot retry with another card. That is deliberate, because a retry could charge twice. An attempt unresolved for over an hour logs a warning for a human.

## 4. Webhook design

- **Events:** `invoice.created`, `invoice.paid`, `invoice.payment_failed`. The payload is the invoice as it was at event time, not as it is when a retry fires.
- **Decoupling:** the API only inserts rows: one event and one delivery per enabled endpoint, in the same transaction as the state change (transactional outbox). If the transaction rolls back there is no event, and if it commits the event is guaranteed to be delivered eventually. The worker is a separate process, so a slow customer endpoint cannot touch API latency.
- **Worker:** claims due rows with `FOR UPDATE SKIP LOCKED` and leases them by pushing `next_attempt_at` 60 s ahead, in a short transaction. It sends with no transaction open. If a worker dies mid-send, the lease expires and another worker retries. Several workers can run safely.
- **Signing:** HMAC-SHA256 over `"<timestamp>.<raw body>"`, keyed with a per-endpoint secret (`whsec_` + 32 random bytes, shown once). Headers: `X-Event-Id`, `X-Timestamp`, `X-Signature: v1=<hex>`.
- **Replay protection:** the timestamp is inside the signed string, the receiver rejects anything older than 5 minutes, and a fresh timestamp is generated for every attempt so late retries still verify. Receivers dedupe on `X-Event-Id`.
- **Retries:** success is any 2xx within 5 s. Otherwise 8 attempts in total, waiting 30 s, 2 min, 10 min, 1 h, 3 h, 6 h and 12 h (±20% jitter), roughly 22 hours. Redirects are not followed.
- **Exhausted:** the delivery becomes `failed` (terminal) and the event stays stored. A business reconciles with `GET /v1/events`, paging back with `starting_after` until it reaches an event it already processed.
- **Guarantee:** at-least-once, so duplicates are possible and receivers must dedupe.

## 5. API key model

- **Format:** `sk_live_<prefix>_<secret>`. The prefix is 8 hex characters. The secret is 32 bytes from the OS CSPRNG.
- **Storage:** the prefix in plaintext (unique, used for lookup) and a SHA-256 hash of the full key. The key is never stored or logged. A fast hash is fine because the secret is 256 bits of randomness and cannot be brute-forced, and a slow hash would add latency to every request.
- **Transmission:** `Authorization: Bearer` over HTTPS, never in a URL.
- **Auth flow:** look up by prefix, compare hashes in constant time, check `revoked_at`. Every failure returns the same generic `401`. `last_used_at` is updated at most once a minute.
- **Rotation:** a business can hold several active keys. Create a new one, switch over, revoke the old one, with no downtime.
- **Revocation:** setting `revoked_at` takes effect on the next request. The row stays for audit.
- **Blast radius if leaked:** one business only. The holder can create customers and invoices and attempt payments with test tokens, but cannot see other tenants and no real card data exists in the system. Fix: revoke, issue a new key, and review `last_used_at` and logs.

## 6. What I cut and why

1. **`draft` and `uncollectible` states:** no finalize step in the brief, and uncollectible needs overdue detection and dunning.
2. **Redis, Kafka or any broker:** the outbox with `SKIP LOCKED` handles the estimated peak of about 40 deliveries per second, and idempotency in Postgres stays atomic with the attempt.
3. **API key management endpoints, scopes and expiry:** a seed command creates keys, and revocation is a database write.
4. **Webhook resend and endpoint management (update, delete, disable):** `GET /v1/events` lets a business reconcile on its own.
5. **Update and delete for customers and invoices:** voiding covers cancellation.

Refunds and partial payments are out of scope. I would add a `refunds` table tied to a succeeded attempt, a `refunded` state, and `refund.*` events.

## 7. Production readiness gap

1. **Observability:** metrics (PSP latency, unresolved attempts, delivery backlog, failed deliveries), tracing, and alerts. Today there are structured logs and a request ID only.
2. **Abuse protection:** per-key rate limits, and SSRF protection on webhook URLs (block private IP ranges and require https). Today `http://` URLs are accepted so the local demo works.
3. **Secrets and data lifecycle:** encrypt webhook secrets at rest (they must be readable to sign), backups with point-in-time recovery, and retention jobs. The cleanup job for expired idempotency keys and old deliveries is not built.

Also missing: an audit log, refunds, dunning, real PSP callbacks instead of polling, and load testing. Known rough edge: GORM logs the expected unique-violation from a losing concurrent payment as an error line.