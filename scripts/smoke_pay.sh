#!/usr/bin/env bash
# Manual smoke test for the pay flow.
# Usage: KEY=sk_live_... bash scripts/smoke_pay.sh
BASE=${BASE:-http://localhost:8081}
KEY=${KEY:?set KEY to your API key}
H1="Authorization: Bearer $KEY"
H2="Content-Type: application/json"
RUN=$RANDOM

id_of() { sed -E 's/^\{"id":"([^"]+)".*/\1/'; }

brief() { # reads "body\nHTTP_CODE" and prints the code plus status/code fields
  local out code body
  out=$(cat)
  code=${out##*$'\n'}
  body=${out%$'\n'*}
  echo "  HTTP $code  $(echo "$body" | grep -oE '"(code|status)": ?"[^"]*"' | tr '\n' ' ')"
}

pay() { # pay <invoice> <idempotency-key> <card-token>
  curl -s -w '\n%{http_code}' -X POST "$BASE/v1/invoices/$1/pay" \
    -H "$H1" -H "$H2" -H "Idempotency-Key: $2" -d "{\"card_token\":\"$3\"}" | brief
}

void() {
  curl -s -w '\n%{http_code}' -X POST "$BASE/v1/invoices/$1/void" -H "$H1" | brief
}

new_invoice() {
  curl -s -X POST "$BASE/v1/invoices" -H "$H1" -H "$H2" \
    -d "{\"customer_id\":\"$CID\",\"due_date\":\"2026-11-01\",\"line_items\":[{\"description\":\"Pro plan\",\"quantity\":2,\"unit_amount_cents\":4900}]}" | id_of
}

CID=$(curl -s -X POST "$BASE/v1/customers" -H "$H1" -H "$H2" \
  -d "{\"name\":\"Smoke Test\",\"email\":\"smoke$RUN@example.com\"}" | id_of)
if [ ${#CID} -ne 36 ]; then
  echo "could not create a customer. Is the API up and KEY correct?"; exit 1
fi

echo "1. declined card (expect 402 card_declined)"
IID=$(new_invoice)
pay $IID $RUN-a1 tok_card_declined

echo "2. insufficient funds, same invoice, new key (expect 402 insufficient_funds)"
pay $IID $RUN-a2 tok_insufficient_funds

echo "3. success (expect 200, status paid + succeeded)"
pay $IID $RUN-a3 tok_success

echo "4. replay of 3 (expect 200 paid + succeeded, same result)"
pay $IID $RUN-a3 tok_success
echo "   replay header (expect Idempotent-Replayed: true):"
curl -si -X POST "$BASE/v1/invoices/$IID/pay" -H "$H1" -H "$H2" \
  -H "Idempotency-Key: $RUN-a3" -d '{"card_token":"tok_success"}' \
  | grep -i '^idempotent-replayed' || echo "   MISSING"

echo "5. same key, different token (expect 422 idempotency_key_reuse)"
pay $IID $RUN-a3 tok_card_declined

echo "6. pay an already paid invoice, new key (expect 409 invoice_not_payable)"
pay $IID $RUN-a4 tok_success

echo "7. void a paid invoice (expect 409 invalid_state_transition)"
void $IID

echo "8. 8 concurrent pays, same invoice, different keys (expect 1 x 200, 7 x 409)"
IID=$(new_invoice)
( for i in 1 2 3 4 5 6 7 8; do
    curl -s -o /dev/null -w '%{http_code}\n' -X POST "$BASE/v1/invoices/$IID/pay" \
      -H "$H1" -H "$H2" -H "Idempotency-Key: $RUN-c$i" -d '{"card_token":"tok_success"}' &
  done; wait ) | sort | uniq -c
echo "   final statuses (expect paid, and exactly one succeeded attempt):"
curl -s "$BASE/v1/invoices/$IID" -H "$H1" | grep -oE '"status":"[^"]*"' | tr '\n' ' '; echo

echo "9. PSP timeout (expect 202, open + unknown, in about 5s, not 30s)"
IID=$(new_invoice)
start=$(date +%s)
pay $IID $RUN-t1 tok_timeout
echo "   took $(( $(date +%s) - start ))s"

echo "10. pay again while unresolved (expect 409 payment_in_progress)"
pay $IID $RUN-t2 tok_success

echo "11. void while unresolved (expect 409 payment_in_progress)"
void $IID

echo "12. replay the timed-out request (expect 202, still unknown, no new charge)"
pay $IID $RUN-t1 tok_timeout

echo "13. PSP returns 500 (expect 202, open + unknown, immediately)"
IID=$(new_invoice)
pay $IID $RUN-n1 tok_network_error

echo "14. missing Idempotency-Key header (expect 400 invalid_request)"
IID=$(new_invoice)
curl -s -w '\n%{http_code}' -X POST "$BASE/v1/invoices/$IID/pay" \
  -H "$H1" -H "$H2" -d '{"card_token":"tok_success"}' | brief

echo "done"
