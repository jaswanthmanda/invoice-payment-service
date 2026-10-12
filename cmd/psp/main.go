// Mock payment processor. Treated by the invoice service as a real external dependency.
//
//	POST /charges              {"reference","card_token","amount_cents"}
//	GET  /charges/{reference}  look up a charge (used by the reconciler)
//	GET  /health
//
// Outcomes are driven by card_token. Charges are idempotent on `reference`:
// the same reference never charges twice and always returns the same result.
package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
)

type chargeReq struct {
	Reference   string `json:"reference"`
	CardToken   string `json:"card_token"`
	AmountCents int64  `json:"amount_cents"`
}

type chargeResp struct {
	Status string `json:"status"`
	PSPRef string `json:"psp_ref,omitempty"`
	Code   string `json:"code,omitempty"`
}

type charge struct {
	done chan struct{} // closed when the charge has finished processing
	resp chargeResp    // written before done is closed
}

type psp struct {
	mu           sync.Mutex
	charges      map[string]*charge
	normalDelay  time.Duration
	timeoutDelay time.Duration
}

func (p *psp) process(token string) chargeResp {
	switch token {
	case "tok_success":
		time.Sleep(p.normalDelay)
		return chargeResp{Status: "succeeded", PSPRef: uuid.NewString()}
	case "tok_insufficient_funds":
		time.Sleep(p.normalDelay)
		return chargeResp{Status: "failed", Code: "insufficient_funds"}
	case "tok_card_declined":
		time.Sleep(p.normalDelay)
		return chargeResp{Status: "failed", Code: "card_declined"}
	case "tok_timeout":
		time.Sleep(p.timeoutDelay) // slow, but the charge does succeed in the end
		return chargeResp{Status: "succeeded", PSPRef: uuid.NewString()}
	default:
		return chargeResp{Status: "failed", Code: "invalid_token"}
	}
}

func (p *psp) handleCharge(w http.ResponseWriter, r *http.Request) {
	var req chargeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Reference == "" || req.CardToken == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}

	// Simulated outage: nothing is charged, nothing is recorded.
	if req.CardToken == "tok_network_error" {
		slog.Info("charge", "reference", req.Reference, "token", req.CardToken, "result", "http_500")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}

	p.mu.Lock()
	c, exists := p.charges[req.Reference]
	if !exists {
		c = &charge{done: make(chan struct{})}
		p.charges[req.Reference] = c
		// Process in the background so the charge completes even if the
		// caller gives up waiting (this is what a real PSP does).
		go func() {
			c.resp = p.process(req.CardToken)
			close(c.done)
			slog.Info("charge", "reference", req.Reference, "token", req.CardToken, "status", c.resp.Status)
		}()
	}
	p.mu.Unlock()

	select {
	case <-c.done:
		writeJSON(w, http.StatusOK, c.resp)
	case <-r.Context().Done():
		// Caller disconnected. The charge keeps processing.
	}
}

func (p *psp) handleGet(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	c, ok := p.charges[r.PathValue("reference")]
	p.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	select {
	case <-c.done:
		writeJSON(w, http.StatusOK, c.resp)
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "processing"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	timeoutSecs := 30
	if v, err := strconv.Atoi(os.Getenv("PSP_TIMEOUT_SECONDS")); err == nil && v > 0 {
		timeoutSecs = v
	}
	p := &psp{
		charges:      make(map[string]*charge),
		normalDelay:  100 * time.Millisecond,
		timeoutDelay: time.Duration(timeoutSecs) * time.Second,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /charges", p.handleCharge)
	mux.HandleFunc("GET /charges/{reference}", p.handleGet)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	addr := os.Getenv("PSP_ADDR")
	if addr == "" {
		addr = ":9090"
	}
	slog.Info("mock psp listening", "addr", addr, "timeout_delay", p.timeoutDelay.String())
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
