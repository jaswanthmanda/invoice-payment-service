package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"invoice-service/internal/models"
)

const (
	maxAttempts  = 8                // 1 first try + 7 retries
	lease        = 60 * time.Second // a claimed delivery is invisible to other workers this long
	batchSize    = 20
	concurrency  = 10
	pollInterval = time.Second
	sendTimeout  = 5 * time.Second
)

// retryDelays[i] is the wait after attempt i+1 fails (before jitter).
// Total budget: 30s + 2m + 10m + 1h + 3h + 6h + 12h, about 22 hours.
var retryDelays = []time.Duration{
	30 * time.Second, 2 * time.Minute, 10 * time.Minute,
	time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour,
}

// Sign returns the signature header value. Signed string: "<timestamp>.<raw body>".
// The timestamp is inside the signed data, so it can't be altered to dodge replay checks.
func Sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify is what a receiver runs: recompute the HMAC, compare in constant time,
// and reject timestamps outside the tolerance (replay protection).
func Verify(secret, timestamp, signature string, body []byte, tolerance time.Duration, now time.Time) error {
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return errors.New("bad timestamp")
	}
	age := now.Sub(time.Unix(ts, 0))
	if age > tolerance || age < -tolerance {
		return errors.New("timestamp outside tolerance (possible replay)")
	}
	if !hmac.Equal([]byte(Sign(secret, timestamp, body)), []byte(signature)) {
		return errors.New("signature mismatch")
	}
	return nil
}

func backoff(attempt int) time.Duration {
	d := retryDelays[attempt-1]
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64())) // +/-20% jitter
}

type Dispatcher struct {
	DB   *gorm.DB
	HTTP *http.Client
}

func NewDispatcher(db *gorm.DB) *Dispatcher {
	return &Dispatcher{
		DB: db,
		HTTP: &http.Client{
			Timeout: sendTimeout,
			// A redirect is a failure, never followed (also blocks redirect-based SSRF).
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Run polls until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context) {
	slog.Info("webhook dispatcher started")
	for ctx.Err() == nil {
		n, err := d.tick(ctx)
		if err != nil {
			slog.Error("dispatcher tick failed", "err", err)
		}
		if err != nil || n < batchSize { // a full batch means more is probably waiting
			select {
			case <-ctx.Done():
			case <-time.After(pollInterval):
			}
		}
	}
	slog.Info("webhook dispatcher stopped")
}

func (d *Dispatcher) tick(ctx context.Context) (int, error) {
	dels, err := d.claim(ctx)
	if err != nil {
		return 0, err
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, del := range dels {
		sem <- struct{}{}
		wg.Add(1)
		go func(del models.WebhookDelivery) {
			defer wg.Done()
			defer func() { <-sem }()
			d.deliver(ctx, del)
		}(del)
	}
	wg.Wait()
	return len(dels), nil
}

// claim is a short transaction: lock due rows with SKIP LOCKED, push their
// next_attempt_at forward by the lease, commit. No network call happens inside it.
// If this worker dies mid-send, the lease expires and another worker retries.
func (d *Dispatcher) claim(ctx context.Context) ([]models.WebhookDelivery, error) {
	var dels []models.WebhookDelivery
	err := d.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = ? AND next_attempt_at <= ?", "pending", now).
			Order("next_attempt_at").
			Limit(batchSize).
			Find(&dels).Error; err != nil {
			return err
		}
		if len(dels) == 0 {
			return nil
		}
		ids := make([]uuid.UUID, len(dels))
		for i, del := range dels {
			ids[i] = del.ID
		}
		return tx.Model(&models.WebhookDelivery{}).
			Where("id IN ?", ids).
			Update("next_attempt_at", now.Add(lease)).Error
	})
	return dels, err
}

func (d *Dispatcher) deliver(ctx context.Context, del models.WebhookDelivery) {
	var ev models.WebhookEvent
	if err := d.DB.WithContext(ctx).Where("id = ?", del.EventID).Take(&ev).Error; err != nil {
		slog.Error("webhook: load event", "delivery_id", del.ID, "err", err)
		return // stays leased, retried when the lease expires
	}
	var ep models.WebhookEndpoint
	if err := d.DB.WithContext(ctx).Where("id = ?", del.EndpointID).Take(&ep).Error; err != nil {
		slog.Error("webhook: load endpoint", "delivery_id", del.ID, "err", err)
		return
	}

	code, sendErr := d.send(ctx, ep, ev)
	if ctx.Err() != nil {
		return // shutting down: leave it leased, don't count a half-finished attempt
	}
	d.record(ctx, del, code, sendErr)
}

func (d *Dispatcher) send(ctx context.Context, ep models.WebhookEndpoint, ev models.WebhookEvent) (int, error) {
	body := []byte(ev.Payload)
	ts := strconv.FormatInt(time.Now().Unix(), 10) // fresh per attempt, so late retries pass the 5 min window

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "invoice-service-webhooks/1")
	req.Header.Set("X-Event-Id", ev.ID.String())
	req.Header.Set("X-Timestamp", ts)
	req.Header.Set("X-Signature", Sign(ep.Secret, ts, body))

	resp, err := d.HTTP.Do(req)
	if err != nil {
		return 0, err // timeout, refused, DNS
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("endpoint returned HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

func (d *Dispatcher) record(ctx context.Context, del models.WebhookDelivery, code int, sendErr error) {
	attempts := del.AttemptCount + 1
	vals := map[string]any{"attempt_count": attempts}
	if code != 0 {
		vals["last_status_code"] = code
	} else {
		vals["last_status_code"] = nil
	}

	status := "pending"
	switch {
	case sendErr == nil:
		status = "delivered"
		vals["status"] = status
		vals["delivered_at"] = time.Now()
		vals["next_attempt_at"] = nil
		vals["last_error"] = nil
	case attempts >= maxAttempts:
		status = "failed" // budget exhausted: terminal. The event stays in GET /v1/events.
		vals["status"] = status
		vals["next_attempt_at"] = nil
		vals["last_error"] = truncate(sendErr.Error(), 500)
	default:
		vals["next_attempt_at"] = time.Now().Add(backoff(attempts))
		vals["last_error"] = truncate(sendErr.Error(), 500)
	}

	err := d.DB.WithContext(ctx).Model(&models.WebhookDelivery{}).
		Where("id = ? AND status = ?", del.ID, "pending").
		Updates(vals).Error
	if err != nil {
		slog.Error("webhook: record result", "delivery_id", del.ID, "err", err)
		return
	}
	slog.Info("webhook attempt", "delivery_id", del.ID, "event_id", del.EventID,
		"attempt", attempts, "status", status, "http_status", code)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
