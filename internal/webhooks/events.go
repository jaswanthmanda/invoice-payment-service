package webhooks

// Package webhooks implements the outbox pattern.
//
// The API never calls a business's URL. It only writes rows: an event and one
// delivery per endpoint, in the SAME transaction as the invoice state change.
// If that transaction rolls back, no event exists. If it commits, the event is
// guaranteed to be delivered eventually by the worker (see dispatcher.go).

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"invoice-service/internal/models"
)

const (
	EventCreated       = "invoice.created"
	EventPaid          = "invoice.paid"
	EventPaymentFailed = "invoice.payment_failed"
)

// Emit records an event and a pending delivery for each enabled endpoint of the
// business. Always call it with the transaction that changes the invoice.
func Emit(tx *gorm.DB, businessID uuid.UUID, eventType string, inv models.Invoice) error {
	now := time.Now()
	ev := models.WebhookEvent{
		ID:         models.NewID(),
		BusinessID: businessID,
		InvoiceID:  inv.ID,
		Type:       eventType,
	}

	// The invoice is serialized now, so the webhook shows the state at event
	// time even if a retry happens hours later.
	payload, err := json.Marshal(map[string]any{
		"id":         ev.ID,
		"type":       eventType,
		"created_at": now.UTC().Format(time.RFC3339),
		"data":       map[string]any{"invoice": inv},
	})
	if err != nil {
		return err
	}
	ev.Payload = string(payload)
	if err := tx.Create(&ev).Error; err != nil {
		return err
	}

	var endpointIDs []uuid.UUID
	if err := tx.Model(&models.WebhookEndpoint{}).
		Where("business_id = ? AND enabled", businessID).
		Pluck("id", &endpointIDs).Error; err != nil {
		return err
	}
	if len(endpointIDs) == 0 {
		return nil // no endpoints: the event is still stored for GET /v1/events
	}

	deliveries := make([]models.WebhookDelivery, 0, len(endpointIDs))
	for _, id := range endpointIDs {
		deliveries = append(deliveries, models.WebhookDelivery{
			ID:            models.NewID(),
			EventID:       ev.ID,
			EndpointID:    id,
			Status:        "pending",
			NextAttemptAt: &now,
		})
	}
	return tx.Create(&deliveries).Error
}
