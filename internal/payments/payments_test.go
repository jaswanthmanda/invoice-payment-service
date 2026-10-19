package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"invoice-service/internal/auth"
	"invoice-service/internal/customers"
	"invoice-service/internal/db"
	"invoice-service/internal/invoices"
	"invoice-service/internal/models"
	"invoice-service/internal/psp"
)

// ---- fake PSP: counts charges, dedupes on reference like a real PSP ----

type fakePSP struct {
	*httptest.Server
	mu      sync.Mutex
	calls   int               // every POST /charges that reached the PSP
	charged map[string]string // reference -> psp_ref (charges that went through)
	hold    time.Duration     // how long tok_timeout stalls its answer
}

func newFakePSP(t *testing.T) *fakePSP {
	f := &fakePSP{charged: map[string]string{}, hold: 3 * time.Second}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /charges", f.charge)
	mux.HandleFunc("GET /charges/{reference}", f.lookup)
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakePSP) record(ref string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.charged[ref]; ok {
		return p
	}
	p := uuid.NewString()
	f.charged[ref] = p
	return p
}

func (f *fakePSP) charge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reference string `json:"reference"`
		CardToken string `json:"card_token"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	f.mu.Lock()
	f.calls++
	f.mu.Unlock()

	switch req.CardToken {
	case "tok_network_error":
		w.WriteHeader(http.StatusInternalServerError) // nothing is charged
	case "tok_timeout":
		pspRef := f.record(req.Reference) // the charge goes through, the answer is just slow
		select {
		case <-time.After(f.hold):
			writeTestJSON(w, map[string]any{"status": "succeeded", "psp_ref": pspRef})
		case <-r.Context().Done(): // caller gave up
		}
	default: // tok_success
		time.Sleep(100 * time.Millisecond) // widen the window so concurrent requests overlap
		writeTestJSON(w, map[string]any{"status": "succeeded", "psp_ref": f.record(req.Reference)})
	}
}

func (f *fakePSP) lookup(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	p, ok := f.charged[r.PathValue("reference")]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeTestJSON(w, map[string]any{"status": "succeeded", "psp_ref": p})
}

func (f *fakePSP) charges() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.charged) }
func (f *fakePSP) callCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ---- test environment: real handlers, real Postgres, fake PSP ----

type env struct {
	t   *testing.T
	gdb *gorm.DB
	api *httptest.Server
	psp *fakePSP
	key string
	cid string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set (see README: running the tests)")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	gdb, err := db.Open(url)
	if err != nil {
		t.Fatalf("db open: %v", err)
	}

	f := newFakePSP(t)
	client := psp.New(f.URL, 500*time.Millisecond) // short limit: tok_timeout must not hang us

	authed := auth.Middleware(gdb)
	cust := &customers.Handler{DB: gdb}
	inv := &invoices.Handler{DB: gdb}
	pay := &Handler{DB: gdb, PSP: client}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/customers", authed(http.HandlerFunc(cust.Create)))
	mux.Handle("POST /v1/invoices", authed(http.HandlerFunc(inv.Create)))
	mux.Handle("POST /v1/invoices/{id}/pay", authed(http.HandlerFunc(pay.Pay)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// A fresh business per test, so tests never see each other's data.
	full, prefix, hash, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	biz := models.Business{ID: models.NewID(), Name: "test-" + uuid.NewString()}
	if err := gdb.Create(&biz).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&models.APIKey{ID: models.NewID(), BusinessID: biz.ID, Prefix: prefix, KeyHash: hash}).Error; err != nil {
		t.Fatal(err)
	}

	e := &env{t: t, gdb: gdb, api: srv, psp: f, key: full}
	status, _, body := e.do("POST", "/v1/customers", "", map[string]string{
		"name": "Test Co", "email": uuid.NewString() + "@example.com",
	})
	if status != http.StatusCreated {
		t.Fatalf("create customer: %d %s", status, body)
	}
	e.cid = idOf(body)
	return e
}

// do is safe to call from goroutines (it never calls t.Fatal).
func (e *env) do(method, path, idemKey string, body any) (int, http.Header, []byte) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.api.URL+path, rdr)
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("Content-Type", "application/json")
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Errorf("%s %s: %v", method, path, err)
		return 0, nil, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

func idOf(body []byte) string {
	var v struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &v)
	return v.ID
}

func (e *env) newInvoice() string {
	status, _, body := e.do("POST", "/v1/invoices", "", map[string]any{
		"customer_id": e.cid,
		"due_date":    "2026-11-01",
		"line_items":  []map[string]any{{"description": "Pro plan", "quantity": 2, "unit_amount_cents": 4900}},
	})
	if status != http.StatusCreated {
		e.t.Fatalf("create invoice: %d %s", status, body)
	}
	return idOf(body)
}

func (e *env) invoiceStatus(id string) string {
	var inv models.Invoice
	if err := e.gdb.Where("id = ?", id).Take(&inv).Error; err != nil {
		e.t.Fatal(err)
	}
	return inv.Status
}

func (e *env) attempts(invoiceID string) []models.PaymentAttempt {
	var a []models.PaymentAttempt
	if err := e.gdb.Where("invoice_id = ?", invoiceID).Order("id").Find(&a).Error; err != nil {
		e.t.Fatal(err)
	}
	return a
}

// reconcile runs the real reconciler logic for one attempt.
func (e *env) reconcile(att models.PaymentAttempt) {
	r := &Reconciler{DB: e.gdb, PSP: psp.New(e.psp.URL, 2*time.Second)}
	r.reconcile(context.Background(), att)
}

func sameJSON(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// ---- Test 1: concurrency ----
// N clients pay the same invoice at the same instant, each with its own key.
// Exactly one may succeed, the card is charged once, and the final state is consistent.

func TestConcurrentPaymentsChargeOnce(t *testing.T) {
	e := newEnv(t)
	id := e.newInvoice()

	const n = 10
	codes := make([]int, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i], _, _ = e.do("POST", "/v1/invoices/"+id+"/pay",
				fmt.Sprintf("conc-%d", i), map[string]string{"card_token": "tok_success"})
		}(i)
	}
	close(start)
	wg.Wait()

	ok, conflict := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		}
	}
	if ok != 1 || conflict != n-1 {
		t.Fatalf("want 1x200 and %dx409, got %v", n-1, codes)
	}
	if got := e.psp.charges(); got != 1 {
		t.Fatalf("PSP charged %d times, want exactly 1", got)
	}
	if s := e.invoiceStatus(id); s != models.InvoicePaid {
		t.Fatalf("invoice status = %q, want paid", s)
	}
	atts := e.attempts(id)
	if len(atts) != 1 || atts[0].Status != models.AttemptSucceeded {
		t.Fatalf("want exactly one succeeded attempt, got %+v", atts)
	}
}

// ---- Test 2: idempotency ----
// Retrying the same request with the same key returns the same response
// and never reaches the PSP a second time.

func TestIdempotentRetryReplaysWithoutSecondCharge(t *testing.T) {
	e := newEnv(t)
	id := e.newInvoice()
	path := "/v1/invoices/" + id + "/pay"
	body := map[string]string{"card_token": "tok_success"}

	s1, _, b1 := e.do("POST", path, "idem-1", body)
	s2, h2, b2 := e.do("POST", path, "idem-1", body)

	if s1 != http.StatusOK || s2 != http.StatusOK {
		t.Fatalf("want 200 and 200, got %d and %d", s1, s2)
	}
	if !sameJSON(b1, b2) {
		t.Fatalf("replayed body differs:\n%s\n%s", b1, b2)
	}
	if h2.Get("Idempotent-Replayed") != "true" {
		t.Fatal("replay is missing the Idempotent-Replayed header")
	}
	if got := e.psp.callCount(); got != 1 {
		t.Fatalf("PSP called %d times, want 1", got)
	}
	if n := len(e.attempts(id)); n != 1 {
		t.Fatalf("want 1 attempt, got %d", n)
	}

	// Same key with a different request is refused, still without touching the PSP.
	s3, _, _ := e.do("POST", path, "idem-1", map[string]string{"card_token": "tok_other"})
	if s3 != http.StatusUnprocessableEntity {
		t.Fatalf("key reuse with a different body: want 422, got %d", s3)
	}
	if got := e.psp.callCount(); got != 1 {
		t.Fatalf("PSP called %d times after key reuse, want 1", got)
	}
}

// ---- Test 3a: PSP timeout ----
// The endpoint must not hang, the invoice must not be corrupted, nobody can
// double-charge while the outcome is unknown, and the reconciler finishes the job.

func TestPSPTimeoutDoesNotCorruptInvoice(t *testing.T) {
	e := newEnv(t)
	id := e.newInvoice()
	path := "/v1/invoices/" + id + "/pay"

	begin := time.Now()
	status, _, _ := e.do("POST", path, "to-1", map[string]string{"card_token": "tok_timeout"})
	if d := time.Since(begin); d > 2*time.Second { // the PSP stalls 3s, our client gives up at 500ms
		t.Fatalf("endpoint hung for %s", d)
	}
	if status != http.StatusAccepted {
		t.Fatalf("want 202, got %d", status)
	}
	if s := e.invoiceStatus(id); s != models.InvoiceOpen {
		t.Fatalf("invoice status = %q, want open", s)
	}
	atts := e.attempts(id)
	if len(atts) != 1 || atts[0].Status != models.AttemptUnknown {
		t.Fatalf("want one unknown attempt, got %+v", atts)
	}

	// While the outcome is unknown, a second payment is refused.
	if s, _, _ := e.do("POST", path, "to-2", map[string]string{"card_token": "tok_success"}); s != http.StatusConflict {
		t.Fatalf("second payment while unknown: want 409, got %d", s)
	}

	// The reconciler asks the PSP with the same reference and completes the payment.
	e.reconcile(atts[0])
	if s := e.invoiceStatus(id); s != models.InvoicePaid {
		t.Fatalf("after reconcile: invoice status = %q, want paid", s)
	}
	if got := e.attempts(id)[0].Status; got != models.AttemptSucceeded {
		t.Fatalf("after reconcile: attempt status = %q, want succeeded", got)
	}

	// The caller retrying with the original key now gets the final answer.
	if s, _, _ := e.do("POST", path, "to-1", map[string]string{"card_token": "tok_timeout"}); s != http.StatusOK {
		t.Fatalf("retry with the original key: want 200, got %d", s)
	}
	if got := e.psp.charges(); got != 1 {
		t.Fatalf("PSP charged %d times, want exactly 1", got)
	}
}

// ---- Test 3b: PSP network error ----
// The PSP answers 500 and has no record of the charge. The invoice must stay
// open, and once the reconciler is sure nothing was charged it is payable again.

func TestPSPNetworkErrorLeavesInvoicePayable(t *testing.T) {
	e := newEnv(t)
	id := e.newInvoice()
	path := "/v1/invoices/" + id + "/pay"

	status, _, _ := e.do("POST", path, "ne-1", map[string]string{"card_token": "tok_network_error"})
	if status != http.StatusAccepted {
		t.Fatalf("want 202, got %d", status)
	}
	if s := e.invoiceStatus(id); s != models.InvoiceOpen {
		t.Fatalf("invoice status = %q, want open", s)
	}
	if a := e.attempts(id); len(a) != 1 || a[0].Status != models.AttemptUnknown {
		t.Fatalf("want one unknown attempt, got %+v", a)
	}

	// Skip the 5 minute grace period by backdating the attempt.
	if err := e.gdb.Model(&models.PaymentAttempt{}).Where("invoice_id = ?", id).
		Update("created_at", time.Now().Add(-10*time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	e.reconcile(e.attempts(id)[0])

	got := e.attempts(id)[0]
	if got.Status != models.AttemptFailed || got.FailureCode == nil || *got.FailureCode != "psp_no_record" {
		t.Fatalf("want failed/psp_no_record, got %+v", got)
	}
	if s := e.invoiceStatus(id); s != models.InvoiceOpen {
		t.Fatalf("invoice status = %q, want open", s)
	}

	// The invoice can be paid again, and is charged exactly once.
	if s, _, _ := e.do("POST", path, "ne-2", map[string]string{"card_token": "tok_success"}); s != http.StatusOK {
		t.Fatalf("pay after release: want 200, got %d", s)
	}
	if s := e.invoiceStatus(id); s != models.InvoicePaid {
		t.Fatalf("invoice status = %q, want paid", s)
	}
	if got := e.psp.charges(); got != 1 {
		t.Fatalf("PSP charged %d times, want exactly 1", got)
	}
}