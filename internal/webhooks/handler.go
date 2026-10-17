package webhooks

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"gorm.io/gorm"

	"invoice-service/internal/auth"
	"invoice-service/internal/httpx"
	"invoice-service/internal/models"
)

type Handler struct {
	DB *gorm.DB
}

type createEndpointRequest struct {
	URL string `json:"url"`
}

// CreateEndpoint handles POST /v1/webhook-endpoints.
// The signing secret is returned once, here, and never again.
func (h *Handler) CreateEndpoint(w http.ResponseWriter, r *http.Request) {
	var req createEndpointRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	u := strings.TrimSpace(req.URL)
	if msg := validateURL(u); msg != "" {
		httpx.Unprocessable(w, "Validation failed", map[string]any{"url": msg})
		return
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		httpx.Internal(w, err)
		return
	}
	secret := "whsec_" + hex.EncodeToString(raw)

	ep := models.WebhookEndpoint{
		ID:         models.NewID(),
		BusinessID: auth.BusinessID(r.Context()),
		URL:        u,
		Secret:     secret,
		Enabled:    true,
	}
	if err := h.DB.WithContext(r.Context()).Create(&ep).Error; err != nil {
		httpx.Internal(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"id":         ep.ID,
		"url":        ep.URL,
		"enabled":    ep.Enabled,
		"created_at": ep.CreatedAt,
		"secret":     secret, // shown once
	})
}

// ListEvents handles GET /v1/events. This is how a business reconciles missed
// webhooks: it lists every event regardless of delivery state, newest first.
// To catch up, page back (starting_after) until you reach an event ID you have
// already processed.
func (h *Handler) ListEvents(w http.ResponseWriter, r *http.Request) {
	page, ok := httpx.ParsePage(w, r)
	if !ok {
		return
	}

	q := h.DB.WithContext(r.Context()).
		Where("business_id = ?", auth.BusinessID(r.Context()))
	if page.After != nil {
		q = q.Where("id < ?", *page.After)
	}

	var events []models.WebhookEvent
	if err := q.Order("id DESC").Limit(page.Limit + 1).Find(&events).Error; err != nil {
		httpx.Internal(w, err)
		return
	}
	hasMore := len(events) > page.Limit
	if hasMore {
		events = events[:page.Limit]
	}

	// Each item is exactly the body that was (or will be) delivered.
	out := make([]json.RawMessage, 0, len(events))
	for _, e := range events {
		out = append(out, json.RawMessage(e.Payload))
	}
	httpx.WriteList(w, out, hasMore)
}

// validateURL accepts http and https so the local demo can use plain http.
// Production would require https and block private IP ranges (SSRF).
func validateURL(raw string) string {
	if raw == "" || len(raw) > 2048 {
		return "must be 1 to 2048 characters"
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "must be an absolute http(s) URL"
	}
	return ""
}
