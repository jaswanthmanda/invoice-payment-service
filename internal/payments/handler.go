// Package payments implements POST /v1/invoices/{id}/pay.
//
// Three phases, matching the design:
//
//	Tx 1 (short): claim the idempotency key, lock the invoice row, insert a
//	              'pending' attempt, commit. The lock is released here.
//	PSP call:     no transaction, no lock held. Hard 5s timeout (psp.Client).
//	Tx 2 (short): record the outcome, update the invoice (status-conditional),
//	              store the response under the idempotency key.
package payments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"invoice-service/internal/auth"
	"invoice-service/internal/dberr"
	"invoice-service/internal/httpx"
	"invoice-service/internal/invoices"
	"invoice-service/internal/models"
	"invoice-service/internal/psp"
)

const (
	idempotencyTTL     = 24 * time.Hour
	inflightConstraint = "payment_attempts_one_inflight"
)

type Handler struct {
	DB  *gorm.DB
	PSP *psp.Client
}

type payRequest struct {
	CardToken string `json:"card_token"`
}

// result is a fully serialized HTTP response. Keeping the bytes lets us store
// the exact response for idempotent replay.
type result struct {
	status   int
	body     []byte
	replayed bool
}

// exit carries an early response out of a transaction. Returning an error from
// the transaction function rolls it back, so nothing is persisted for it.
type exit struct{ res result }

func (e *exit) Error() string { return "early exit" }

func errResult(status int, code, message string, details map[string]any) result {
	return result{status: status, body: httpx.ErrorBytes(code, message, details)}
}

func jsonResult(status int, v any) (result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return result{}, err
	}
	return result{status: status, body: b}, nil
}

func failureMessage(code string) string {
	switch code {
	case "card_declined":
		return "The card was declined"
	case "insufficient_funds":
		return "The card has insufficient funds"
	default:
		return "The payment failed"
	}
}

func failedResult(att models.PaymentAttempt, code string) result {
	return errResult(http.StatusPaymentRequired, code, failureMessage(code), map[string]any{
		"payment_attempt_id": att.ID,
		"invoice_status":     models.InvoiceOpen, // a failed payment never changes the invoice
	})
}

// Pay handles POST /v1/invoices/{id}/pay.
func (h *Handler) Pay(w http.ResponseWriter, r *http.Request) {
	invoiceID, ok := httpx.PathID(r)
	if !ok {
		httpx.NotFound(w, "Invoice not found")
		return
	}

	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 255 {
		httpx.BadRequest(w, "Idempotency-Key header is required (1 to 255 characters)")
		return
	}

	var req payRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	token := strings.TrimSpace(req.CardToken)
	if token == "" || len(token) > 100 {
		httpx.Unprocessable(w, "Validation failed", map[string]any{"card_token": "must be 1 to 100 characters"})
		return
	}

	// The hash covers everything that defines "the same request".
	sum := sha256.Sum256([]byte("POST /v1/invoices/" + invoiceID.String() + "/pay\n" + token))
	hash := hex.EncodeToString(sum[:])

	res := h.pay(r.Context(), auth.BusinessID(r.Context()), invoiceID, key, hash, token)
	if res.replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	httpx.WriteRaw(w, res.status, res.body)
}

func (h *Handler) pay(ctx context.Context, bid, invoiceID uuid.UUID, key, hash, token string) result {
	var att models.PaymentAttempt

	// ---- Tx 1: claim key, lock invoice, insert pending attempt ----
	err := h.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now()

		// A key past its TTL counts as new.
		if err := tx.Where("business_id = ? AND key = ? AND expires_at < ?", bid, key, now).
			Delete(&models.IdempotencyKey{}).Error; err != nil {
			return err
		}

		// Claim the key. The primary key (business_id, key) makes this atomic:
		// of two concurrent requests with the same key, exactly one inserts.
		rec := models.IdempotencyKey{
			BusinessID:  bid,
			Key:         key,
			RequestHash: hash,
			ExpiresAt:   now.Add(idempotencyTTL),
		}
		ins := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&rec)
		if ins.Error != nil {
			return ins.Error
		}
		if ins.RowsAffected == 0 {
			return replay(tx, bid, key, hash) // key already used
		}

		// Serialize all pays (and voids) of this invoice. Held only until commit.
		var inv models.Invoice
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND business_id = ?", invoiceID, bid).
			Take(&inv).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return &exit{res: errResult(http.StatusNotFound, "not_found", "Invoice not found", nil)}
		}
		if err != nil {
			return err
		}

		if !invoices.CanTransition(inv.Status, models.InvoicePaid) {
			return &exit{res: errResult(http.StatusConflict, "invoice_not_payable",
				"Invoice is "+inv.Status+" and cannot be paid",
				map[string]any{"current_status": inv.Status})}
		}

		// The amount always comes from the invoice, never from the client.
		att = models.PaymentAttempt{
			ID:          models.NewID(),
			InvoiceID:   inv.ID,
			BusinessID:  bid,
			AmountCents: inv.TotalCents,
			CardToken:   token,
			Status:      models.AttemptPending,
		}
		if err := tx.Create(&att).Error; err != nil {
			// Backstop: the partial unique index allows only one unresolved
			// (pending or unknown) attempt per invoice.
			if c, ok := dberr.UniqueViolation(err); ok && c == inflightConstraint {
				return &exit{res: errResult(http.StatusConflict, "payment_in_progress",
					"A payment for this invoice is already in progress", nil)}
			}
			return err
		}

		return tx.Model(&models.IdempotencyKey{}).
			Where("business_id = ? AND key = ?", bid, key).
			Update("payment_attempt_id", att.ID).Error
	})

	var ex *exit
	switch {
	case errors.As(err, &ex):
		return ex.res
	case err != nil:
		slog.Error("pay: could not start payment", "err", err)
		return errResult(http.StatusInternalServerError, "internal_error", "Internal server error", nil)
	}

	// ---- PSP call: no transaction, no lock ----
	// Detached from the request context so a client that hangs up mid-call
	// doesn't abandon a charge we may already have sent.
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()

	// Our attempt ID is the PSP reference: the same reference never charges twice.
	res := h.PSP.Charge(bg, att.ID.String(), token, att.AmountCents)
	slog.Info("psp charge", "attempt_id", att.ID, "status", res.Status, "err", res.Err)

	return h.finalize(bg, bid, key, att, res)
}

// replay answers a request whose idempotency key already exists.
func replay(tx *gorm.DB, bid uuid.UUID, key, hash string) error {
	var existing models.IdempotencyKey
	if err := tx.Where("business_id = ? AND key = ?", bid, key).Take(&existing).Error; err != nil {
		return err
	}

	// Same key, different request: refuse. No PSP call.
	if existing.RequestHash != hash {
		return &exit{res: errResult(http.StatusUnprocessableEntity, "idempotency_key_reuse",
			"This Idempotency-Key was already used with a different request", nil)}
	}

	// Finished earlier: return the stored response. No second PSP call.
	if existing.ResponseStatus != nil && existing.ResponseBody != nil {
		return &exit{res: result{
			status:   *existing.ResponseStatus,
			body:     []byte(*existing.ResponseBody),
			replayed: true,
		}}
	}

	// Not finished (still processing, or outcome unknown): report the attempt's
	// current state. If the reconciler resolved it since, this returns the final result.
	if existing.PaymentAttemptID == nil {
		return &exit{res: errResult(http.StatusConflict, "request_in_progress",
			"A request with this Idempotency-Key is in progress", nil)}
	}
	res, err := currentResult(tx, bid, *existing.PaymentAttemptID)
	if err != nil {
		return err
	}
	res.replayed = true
	return &exit{res: res}
}

// finalize is Tx 2: record the PSP outcome.
func (h *Handler) finalize(ctx context.Context, bid uuid.UUID, key string, att models.PaymentAttempt, res psp.Result) result {
	var out result

	err := h.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now()

		var (
			ok  bool
			err error
		)
		switch res.Status {
		case psp.Succeeded:
			ok, err = resolveAttempt(tx, att.ID, map[string]any{
				"status": models.AttemptSucceeded, "psp_ref": res.PSPRef, "completed_at": now,
			})
		case psp.Failed:
			ok, err = resolveAttempt(tx, att.ID, map[string]any{
				"status": models.AttemptFailed, "failure_code": res.Code, "completed_at": now,
			})
		default: // Unknown: timeout, network error, 5xx, bad response
			ok, err = resolveAttempt(tx, att.ID, map[string]any{"status": models.AttemptUnknown})
		}
		if err != nil {
			return err
		}
		if !ok {
			// The attempt is no longer 'pending': the reconciler resolved it first.
			// Report what is true now and change nothing else.
			cur, err := currentResult(tx, bid, att.ID)
			if err != nil {
				return err
			}
			out = cur
			return nil
		}

		store := true // final outcomes are stored for replay
		switch res.Status {
		case psp.Succeeded:
			// Conditional update: only an 'open' invoice can become 'paid'.
			upd := tx.Model(&models.Invoice{}).
				Where("id = ? AND status = ?", att.InvoiceID, models.InvoiceOpen).
				Updates(map[string]any{"status": models.InvoicePaid, "paid_at": now, "updated_at": now})
			if upd.Error != nil {
				return upd.Error
			}
			if upd.RowsAffected != 1 {
				// Cannot happen: void refuses while an attempt is unresolved.
				return fmt.Errorf("invariant violated: invoice %s was not open while its payment was in flight", att.InvoiceID)
			}
			// NEXT STEP: insert the invoice.paid webhook event here, in this same transaction.
			inv, err := loadInvoice(tx, att.InvoiceID, bid)
			if err != nil {
				return err
			}
			out, err = jsonResult(http.StatusOK, inv)
			if err != nil {
				return err
			}

		case psp.Failed:
			// NEXT STEP: insert the invoice.payment_failed webhook event here.
			out = failedResult(att, res.Code)

		default:
			inv, err := loadInvoice(tx, att.InvoiceID, bid)
			if err != nil {
				return err
			}
			out, err = jsonResult(http.StatusAccepted, inv)
			if err != nil {
				return err
			}
			store = false // not final: replays will report the attempt's current state
		}

		if !store {
			return nil
		}
		return tx.Model(&models.IdempotencyKey{}).
			Where("business_id = ? AND key = ?", bid, key).
			Updates(map[string]any{"response_status": out.status, "response_body": string(out.body)}).Error
	})
	if err != nil {
		// The attempt stays 'pending'. The reconciler resolves it, and a retry
		// with the same key reports its state.
		slog.Error("pay: could not record outcome", "attempt_id", att.ID, "err", err)
		return errResult(http.StatusInternalServerError, "internal_error",
			"Payment outcome could not be recorded. Retry with the same Idempotency-Key.", nil)
	}
	return out
}

// resolveAttempt moves an attempt out of 'pending'. It reports false if the
// attempt was no longer pending (someone else resolved it).
func resolveAttempt(tx *gorm.DB, id uuid.UUID, values map[string]any) (bool, error) {
	r := tx.Model(&models.PaymentAttempt{}).
		Where("id = ? AND status = ?", id, models.AttemptPending).
		Updates(values)
	return r.RowsAffected == 1, r.Error
}

// currentResult builds the response that matches an attempt's current state.
func currentResult(db *gorm.DB, bid, attemptID uuid.UUID) (result, error) {
	var att models.PaymentAttempt
	if err := db.Where("id = ? AND business_id = ?", attemptID, bid).Take(&att).Error; err != nil {
		return result{}, err
	}
	if att.Status == models.AttemptFailed {
		code := "payment_failed"
		if att.FailureCode != nil {
			code = *att.FailureCode
		}
		return failedResult(att, code), nil
	}
	inv, err := loadInvoice(db, att.InvoiceID, bid)
	if err != nil {
		return result{}, err
	}
	if att.Status == models.AttemptSucceeded {
		return jsonResult(http.StatusOK, inv)
	}
	return jsonResult(http.StatusAccepted, inv) // pending or unknown: still unresolved
}

func loadInvoice(db *gorm.DB, id, businessID uuid.UUID) (models.Invoice, error) {
	var inv models.Invoice
	err := db.
		Preload("LineItems", func(d *gorm.DB) *gorm.DB { return d.Order("id") }).
		Preload("PaymentAttempts", func(d *gorm.DB) *gorm.DB { return d.Order("id") }).
		Where("id = ? AND business_id = ?", id, businessID).
		Take(&inv).Error
	return inv, err
}
