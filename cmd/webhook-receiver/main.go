package main

// Webhook receiver for local testing. It plays the role of a business's endpoint:
// verifies the signature, rejects stale timestamps, and dedupes on event ID.
//
//	WEBHOOK_SECRET=whsec_... go run ./cmd/webhook-receiver
//	FAIL_FIRST=2 ...   answers the first 2 requests with HTTP 500 (to see retries)

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"invoice-service/internal/webhooks"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	secret := os.Getenv("WEBHOOK_SECRET")
	if secret == "" {
		slog.Warn("WEBHOOK_SECRET not set: signatures will NOT be verified")
	}
	failFirst, _ := strconv.Atoi(os.Getenv("FAIL_FIRST"))
	addr := os.Getenv("RECEIVER_ADDR")
	if addr == "" {
		addr = ":9100"
	}

	var requests atomic.Int64
	var mu sync.Mutex
	seen := map[string]bool{}

	http.HandleFunc("POST /hook", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		n := requests.Add(1)

		if n <= int64(failFirst) {
			slog.Info("simulated failure", "request_no", n, "event_id", r.Header.Get("X-Event-Id"))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		verified := "skipped"
		if secret != "" {
			err := webhooks.Verify(secret, r.Header.Get("X-Timestamp"), r.Header.Get("X-Signature"),
				body, 5*time.Minute, time.Now())
			if err != nil {
				slog.Warn("rejected", "reason", err.Error(), "event_id", r.Header.Get("X-Event-Id"))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			verified = "valid"
		}

		eventID := r.Header.Get("X-Event-Id")
		mu.Lock()
		dup := seen[eventID]
		seen[eventID] = true
		mu.Unlock()

		var ev struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(body, &ev)

		shown := string(body)
		if len(shown) > 300 {
			shown = shown[:300] + "..."
		}
		slog.Info("webhook received",
			"request_no", n, "event_id", eventID, "type", ev.Type,
			"signature", verified, "duplicate", dup, "body", shown)
		w.WriteHeader(http.StatusOK)
	})

	slog.Info("webhook receiver listening", "addr", addr, "fail_first", failFirst)
	if err := http.ListenAndServe(addr, nil); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
