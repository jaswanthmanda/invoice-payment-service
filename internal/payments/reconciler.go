package payments

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"gorm.io/gorm"

	"invoice-service/internal/models"
	"invoice-service/internal/psp"
	"invoice-service/internal/webhooks"
)

const (
	reconcileInterval = 15 * time.Second
	pendingMinAge     = 60 * time.Second // a live pay request finishes or times out well inside this
	unknownMinAge     = 15 * time.Second
	notFoundGrace     = 5 * time.Minute // PSP has no record: wait this long before concluding "never charged"
	staleWarnAfter    = time.Hour
	reconcileBatch    = 25
)

// Reconciler resolves payment attempts that a pay request could not finish:
// 'unknown' (PSP timeout or error) and 'pending' (the API crashed mid-payment).
// It asks the PSP about each one using the same reference the charge used, so
// it can never cause a second charge.
type Reconciler struct {
	DB  *gorm.DB
	PSP *psp.Client
}

func (r *Reconciler) Run(ctx context.Context) {
	slog.Info("payment reconciler started")
	t := time.NewTicker(reconcileInterval)
	defer t.Stop()
	for {
		r.tick(ctx)
		select {
		case <-ctx.Done():
			slog.Info("payment reconciler stopped")
			return
		case <-t.C:
		}
	}
}

func (r *Reconciler) tick(ctx context.Context) {
	now := time.Now()
	var atts []models.PaymentAttempt
	err := r.DB.WithContext(ctx).
		Where("(status = ? AND created_at < ?) OR (status = ? AND created_at < ?)",
			models.AttemptPending, now.Add(-pendingMinAge),
			models.AttemptUnknown, now.Add(-unknownMinAge)).
		Order("created_at").
		Limit(reconcileBatch).
		Find(&atts).Error
	if err != nil {
		slog.Error("reconciler: query failed", "err", err)
		return
	}
	for _, att := range atts {
		if ctx.Err() != nil {
			return
		}
		r.reconcile(ctx, att)
	}
}

func (r *Reconciler) reconcile(ctx context.Context, att models.PaymentAttempt) {
	age := time.Since(att.CreatedAt)
	res := r.PSP.Lookup(ctx, att.ID.String()) // same reference the charge used

	switch res.Status {
	case psp.Succeeded, psp.Failed:
		// Definitive answer from the PSP: act on it, whatever the age.
	case psp.NotFound:
		// The PSP never recorded this charge (our API died before sending it, or
		// the request never arrived). Only conclude "never charged" after a grace
		// period, in case a delayed request is still on its way.
		if age < notFoundGrace {
			return
		}
		res = psp.Result{Status: psp.Failed, Code: "psp_no_record"}
	default:
		// Still processing, or the PSP is unreachable: try again next tick.
		if age > staleWarnAfter {
			slog.Warn("payment attempt unresolved for over an hour: needs a human",
				"attempt_id", att.ID, "invoice_id", att.InvoiceID, "age", age.String())
		}
		return
	}

	if err := r.apply(ctx, att, res); err != nil {
		slog.Error("reconciler: could not apply result", "attempt_id", att.ID, "err", err)
	}
}

// apply records a definitive outcome. It mirrors the pay flow's second
// transaction: attempt first, then invoice, event, and stored idempotent response.
func (r *Reconciler) apply(ctx context.Context, att models.PaymentAttempt, res psp.Result) error {
	resolved := false

	err := r.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now()

		values := map[string]any{"completed_at": now}
		if res.Status == psp.Succeeded {
			values["status"] = models.AttemptSucceeded
			values["psp_ref"] = res.PSPRef
		} else {
			values["status"] = models.AttemptFailed
			values["failure_code"] = res.Code
		}

		// First writer wins: this matches only while the attempt is unresolved,
		// so the pay request and the reconciler can never both apply a result.
		won := tx.Model(&models.PaymentAttempt{}).
			Where("id = ? AND status IN ?", att.ID, []string{models.AttemptPending, models.AttemptUnknown}).
			Updates(values)
		if won.Error != nil {
			return won.Error
		}
		if won.RowsAffected == 0 {
			return nil // already resolved by someone else
		}
		resolved = true

		var out result
		if res.Status == psp.Succeeded {
			paid := tx.Model(&models.Invoice{}).
				Where("id = ? AND status = ?", att.InvoiceID, models.InvoiceOpen).
				Updates(map[string]any{"status": models.InvoicePaid, "paid_at": now, "updated_at": now})
			if paid.Error != nil {
				return paid.Error
			}
			if paid.RowsAffected != 1 {
				// Cannot happen: void refuses while an attempt is unresolved.
				return fmt.Errorf("invariant violated: invoice %s was not open while its payment was unresolved", att.InvoiceID)
			}
			inv, err := loadInvoice(tx, att.InvoiceID, att.BusinessID)
			if err != nil {
				return err
			}
			if err := webhooks.Emit(tx, att.BusinessID, webhooks.EventPaid, inv); err != nil {
				return err
			}
			if out, err = jsonResult(http.StatusOK, inv); err != nil {
				return err
			}
		} else {
			inv, err := loadInvoice(tx, att.InvoiceID, att.BusinessID)
			if err != nil {
				return err
			}
			if err := webhooks.Emit(tx, att.BusinessID, webhooks.EventPaymentFailed, inv); err != nil {
				return err
			}
			out = failedResult(att, res.Code)
		}

		// A caller retrying with the original Idempotency-Key now gets the final result.
		return tx.Model(&models.IdempotencyKey{}).
			Where("payment_attempt_id = ? AND response_status IS NULL", att.ID).
			Updates(map[string]any{"response_status": out.status, "response_body": string(out.body)}).Error
	})

	if err == nil && resolved {
		slog.Info("payment reconciled", "attempt_id", att.ID, "invoice_id", att.InvoiceID,
			"result", res.Status, "code", res.Code)
	}
	return err
}
