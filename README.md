# Invoice & Payment Service

A small billing backend: a business creates customers and invoices, customers pay invoices, and the business is notified of state changes by signed webhooks. Written in Go with PostgreSQL.

- **Design document:** [DESIGN.md](DESIGN.md) (state machine, failure modes, webhooks, API keys, what was cut)
- **API reference:** [docs/openapi.yaml](docs/openapi.yaml)
- **How AI was used:** [AI_USAGE.md](AI_USAGE.md)

## Demo Video

<paste video link here> (no login needed)

## Why Go

Rust was preferred. I chose Go because it is the language I know best, so I can explain every line of the payment path on camera. The design is language independent: short transactions, row locks, a partial unique index, status-conditional updates, and hard timeouts on the PSP call.

## Run it

Requires Docker with Compose. Nothing else.

```bash
git clone <your repo url> && cd invoice-payment-service
docker compose up -d --build
docker compose ps
```

After about 30 seconds `db`, `psp` and `api` show `healthy`, and `worker` is up. The `seed` service runs once, creates a demo business, and prints its API key (shown once):

```bash
docker compose logs seed
```

| Service | What it is | Host port |
|---|---|---|
| `api` | HTTP API | 8080 |
| `worker` | webhook dispatcher and payment reconciler | none |
| `psp` | mock payment processor | 9090 |
| `db` | PostgreSQL 16 (migrations run on startup) | 5433 |

If a port is busy, change it for one run: `API_PORT=8082 PSP_PORT=9091 DB_PORT=5434 docker compose up -d`.

Another API key for the same or a new business:

```bash
docker compose run --rm seed /app/seed -name "Another Business"
docker compose run --rm seed /app/seed -business-id <business uuid>
```

Reset everything (deletes data): `docker compose down -v`.

## Try it with curl

Set the key from the seed output:

```bash
export KEY=$(docker compose logs seed | grep -o 'sk_live_[A-Za-z0-9_]*' | head -1)
H1="Authorization: Bearer $KEY"; H2="Content-Type: application/json"
id_of() { sed -E 's/^\{"id":"([^"]+)".*/\1/'; }
```

**1. Create a customer**

```bash
CID=$(curl -s -X POST localhost:8080/v1/customers -H "$H1" -H "$H2" \
  -d '{"name":"Acme Ltd","email":"billing@acme.com"}' | id_of)
echo $CID
```

**2. Create an invoice.** The server computes the total (2 x 4900 + 1 x 1500 = 11300 cents):

```bash
IID=$(curl -s -X POST localhost:8080/v1/invoices -H "$H1" -H "$H2" -d '{
  "customer_id": "'$CID'",
  "due_date": "2026-11-01",
  "line_items": [
    {"description": "Pro plan", "quantity": 2, "unit_amount_cents": 4900},
    {"description": "Setup",    "quantity": 1, "unit_amount_cents": 1500}
  ]}' | id_of)
curl -s localhost:8080/v1/invoices/$IID -H "$H1"
```

**3. Payment fails.** Returns `402`. The invoice stays `open` and can be retried:

```bash
curl -s -X POST localhost:8080/v1/invoices/$IID/pay -H "$H1" -H "$H2" \
  -H "Idempotency-Key: demo-1" -d '{"card_token":"tok_card_declined"}'
```

**4. Payment succeeds.** Returns `200` with the invoice `paid`:

```bash
curl -s -X POST localhost:8080/v1/invoices/$IID/pay -H "$H1" -H "$H2" \
  -H "Idempotency-Key: demo-2" -d '{"card_token":"tok_success"}'
```

Send step 4 again with the same `Idempotency-Key` and you get the same body back with `Idempotent-Replayed: true`, and no second charge.

Test card tokens: `tok_success`, `tok_card_declined`, `tok_insufficient_funds`, `tok_timeout` (the PSP stalls 30 s; the API answers `202` after 5 s and the worker resolves it), `tok_network_error` (the PSP returns HTTP 500).

### Full scenario script

`scripts/smoke_pay.sh` runs 14 checks against a live stack: declines, success, replay, key reuse, paying a paid invoice, 8 concurrent payments (exactly one succeeds), PSP timeout, PSP error, missing header. Each line prints what it expects. Needs bash.

```bash
KEY=<api key> BASE=http://localhost:8080 bash scripts/smoke_pay.sh
```

### Webhooks

```bash
# 1. register an endpoint (the signing secret is returned once)
curl -s -X POST localhost:8080/v1/webhook-endpoints -H "$H1" -H "$H2" \
  -d '{"url":"http://receiver:9100/hook"}'

# 2. start the demo receiver (it verifies signatures and dedupes by event id)
WEBHOOK_SECRET=whsec_<secret from step 1> docker compose --profile demo up -d receiver
docker compose logs -f receiver
```

Create or pay an invoice and the receiver logs `invoice.created`, `invoice.paid` or `invoice.payment_failed`, each with `"signature":"valid"`. Set `FAIL_FIRST=2` on the receiver to watch the retries: the first attempt fails, the next comes about 30 seconds later, the third about 2 minutes after that. `GET /v1/events` lists every event for reconciliation.

## Tests

Three tests are required by the brief, plus one extra. They run the real handlers against a real PostgreSQL and a fake PSP that counts charges.

| Test | What it asserts |
|---|---|
| `TestConcurrentPaymentsChargeOnce` | 10 simultaneous pays: exactly one `200`, nine `409`, one PSP charge, one succeeded attempt, invoice `paid` |
| `TestIdempotentRetryReplaysWithoutSecondCharge` | same key and body returns the identical response with the replay header and no second PSP call; same key with a different body returns `422` |
| `TestPSPTimeoutDoesNotCorruptInvoice` | endpoint returns `202` quickly, invoice stays `open`, a second pay is blocked, the reconciler completes it, and the original key then returns `200` with one charge |
| `TestPSPNetworkErrorLeavesInvoicePayable` | invoice stays `open`, the reconciler releases it, and a later pay succeeds once |

```bash
docker compose up -d db
docker compose exec db psql -U postgres -c "create database invoices_test"
export TEST_DATABASE_URL="postgres://postgres:pass@localhost:5433/invoices_test?sslmode=disable"
go test ./internal/payments/ -v -count=1
```

Without `TEST_DATABASE_URL` the tests skip. No required test was skipped.

## Configuration

| Variable | Default | Used by |
|---|---|---|
| `DATABASE_URL` | none, required | api, worker, seed |
| `PSP_URL` | `http://localhost:9090` | api, worker |
| `API_ADDR` | `:8080` | api |
| `PSP_ADDR` | `:9090` | psp |
| `PSP_TIMEOUT_SECONDS` | `30` | psp (length of the `tok_timeout` stall) |

## Layout

```
cmd/api               HTTP server
cmd/worker            webhook dispatcher and payment reconciler
cmd/psp               mock payment processor
cmd/seed              creates a business and an API key
cmd/webhook-receiver  demo webhook endpoint
internal/             auth, customers, invoices, payments, webhooks, psp client, httpx
migrations/           SQL migrations (embedded, applied on startup)
docs/openapi.yaml     API reference
scripts/              smoke_pay.sh
```

## Notes

- Money is integer cents everywhere (`BIGINT` and `int64`). There are no floats in the money path.
- Invoice states are `open`, `paid` and `void`. Paid and void are terminal, and a failed payment never changes the invoice.
- While a payment outcome is unknown, further payments and voids on that invoice are refused (`409 payment_in_progress`) until the reconciler learns the result. This is deliberate: retrying could charge twice.
- Out of scope and not built: subscriptions, refunds and partial payments, multi-currency, tax, UI, email, OAuth, rate limiting. See DESIGN.md for what was cut and what is missing for production.