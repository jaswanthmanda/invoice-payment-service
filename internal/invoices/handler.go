package invoices

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"invoice-service/internal/auth"
	"invoice-service/internal/httpx"
	"invoice-service/internal/models"
	"invoice-service/internal/webhooks"
)

// Limits keep money math safe from int64 overflow:
// 100 items * 1,000,000 qty * 10,000,000,000 cents = 1e18, below int64 max (~9.2e18).
const (
	maxLineItems       = 100
	maxQuantity        = 1_000_000
	maxUnitAmountCents = 10_000_000_000 // $100M
)

// allowed is the whole state machine. paid and void have no entries: terminal.
var allowed = map[string][]string{
	models.InvoiceOpen: {models.InvoicePaid, models.InvoiceVoid},
}

// CanTransition is the single place that decides whether a transition is valid.
func CanTransition(from, to string) bool {
	for _, t := range allowed[from] {
		if t == to {
			return true
		}
	}
	return false
}

type stateError struct{ current string }

func (e *stateError) Error() string { return "invalid state transition from " + e.current }

var errPaymentInFlight = errors.New("payment in flight")

type Handler struct {
	DB *gorm.DB
}

type lineItemRequest struct {
	Description     string `json:"description"`
	Quantity        int    `json:"quantity"`
	UnitAmountCents int64  `json:"unit_amount_cents"`
}

type createRequest struct {
	CustomerID uuid.UUID         `json:"customer_id"`
	DueDate    models.Date       `json:"due_date"`
	LineItems  []lineItemRequest `json:"line_items"`
}

func lineItemsInOrder(db *gorm.DB) *gorm.DB { return db.Order("id") }

func attemptsInOrder(db *gorm.DB) *gorm.DB { return db.Order("id") }

// validateCreate returns the server-computed total and any validation problems.
func validateCreate(req *createRequest) (int64, map[string]any) {
	problems := map[string]any{}

	if req.CustomerID == uuid.Nil {
		problems["customer_id"] = "is required"
	}
	if req.DueDate.IsZero() {
		problems["due_date"] = "is required (YYYY-MM-DD)"
	}
	if n := len(req.LineItems); n < 1 || n > maxLineItems {
		problems["line_items"] = "must contain 1 to 100 items"
		return 0, problems
	}

	var total int64
	for i, li := range req.LineItems {
		key := "line_items[" + strconv.Itoa(i) + "]"
		d := strings.TrimSpace(li.Description)
		ok := true
		if d == "" || len(d) > 500 {
			problems[key+".description"] = "must be 1 to 500 characters"
			ok = false
		}
		if li.Quantity < 1 || li.Quantity > maxQuantity {
			problems[key+".quantity"] = "must be between 1 and 1000000"
			ok = false
		}
		if li.UnitAmountCents < 0 || li.UnitAmountCents > maxUnitAmountCents {
			problems[key+".unit_amount_cents"] = "must be between 0 and 10000000000"
			ok = false
		}
		if ok {
			total += int64(li.Quantity) * li.UnitAmountCents // integer cents only
		}
	}
	if len(problems) == 0 && total <= 0 {
		problems["line_items"] = "invoice total must be greater than zero"
	}
	return total, problems
}

// Create handles POST /v1/invoices. The total is always computed here.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	total, problems := validateCreate(&req)
	if len(problems) > 0 {
		httpx.Unprocessable(w, "Validation failed", problems)
		return
	}

	bid := auth.BusinessID(r.Context())
	inv := models.Invoice{
		ID:         models.NewID(),
		BusinessID: bid,
		CustomerID: req.CustomerID,
		Status:     models.InvoiceOpen,
		TotalCents: total,
		Currency:   "USD",
		DueDate:    req.DueDate,
	}
	for _, li := range req.LineItems {
		inv.LineItems = append(inv.LineItems, models.InvoiceLineItem{
			ID:              models.NewID(),
			InvoiceID:       inv.ID,
			Description:     strings.TrimSpace(li.Description),
			Quantity:        li.Quantity,
			UnitAmountCents: li.UnitAmountCents,
		})
	}

	inv.PaymentAttempts = []models.PaymentAttempt{} // new invoice: serialize as [] not null

	err := h.DB.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		// The customer must belong to the same business.
		var c models.Customer
		if err := tx.Select("id").
			Where("id = ? AND business_id = ?", req.CustomerID, bid).
			Take(&c).Error; err != nil {
			return err
		}

		if err := tx.Create(&inv).Error; err != nil { // invoice + line items
			return err
		}
		// Same transaction as the invoice: the event exists if and only if the invoice does.
		return webhooks.Emit(tx, bid, webhooks.EventCreated, inv)
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		httpx.Unprocessable(w, "Validation failed", map[string]any{"customer_id": "customer not found"})
		return
	}
	if err != nil {
		httpx.Internal(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, inv)
}

func (h *Handler) load(ctx context.Context, id, businessID uuid.UUID) (models.Invoice, error) {
	var inv models.Invoice
	// err := h.DB.WithContext(ctx).
	// 	Preload("LineItems", lineItemsInOrder).
	// 	Where("id = ? AND business_id = ?", id, businessID).
	// 	Take(&inv).Error

	// if err != nil {
	// 	return inv, err
	// }

	err := h.DB.WithContext(ctx).
		Preload("LineItems", lineItemsInOrder).
		Preload("PaymentAttempts", attemptsInOrder).
		Where("id = ? AND business_id = ?", id, businessID).
		Take(&inv).Error

	return inv, err
}

// Get handles GET /v1/invoices/{id}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathID(r)
	if !ok {
		httpx.NotFound(w, "Invoice not found")
		return
	}
	inv, err := h.load(r.Context(), id, auth.BusinessID(r.Context()))
	if errors.Is(err, gorm.ErrRecordNotFound) {
		httpx.NotFound(w, "Invoice not found")
		return
	}
	if err != nil {
		httpx.Internal(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, inv)
}

// List handles GET /v1/invoices?status=&limit=&starting_after=.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	page, ok := httpx.ParsePage(w, r)
	if !ok {
		return
	}

	q := h.DB.WithContext(r.Context()).
		Where("business_id = ?", auth.BusinessID(r.Context()))

	if s := r.URL.Query().Get("status"); s != "" {
		if s != models.InvoiceOpen && s != models.InvoicePaid && s != models.InvoiceVoid {
			httpx.BadRequest(w, "status must be one of: open, paid, void")
			return
		}
		q = q.Where("status = ?", s)
	}
	if page.After != nil {
		q = q.Where("id < ?", *page.After)
	}

	invoices := make([]models.Invoice, 0, page.Limit+1)
	err := q.Preload("LineItems", lineItemsInOrder).
		Preload("PaymentAttempts", attemptsInOrder).
		Order("id DESC").
		Limit(page.Limit + 1). // one extra row tells us if another page exists
		Find(&invoices).Error
	if err != nil {
		httpx.Internal(w, err)
		return
	}

	hasMore := len(invoices) > page.Limit
	if hasMore {
		invoices = invoices[:page.Limit]
	}
	httpx.WriteList(w, invoices, hasMore)
}

// Void handles POST /v1/invoices/{id}/void (open -> void).
func (h *Handler) Void(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathID(r)
	if !ok {
		httpx.NotFound(w, "Invoice not found")
		return
	}
	bid := auth.BusinessID(r.Context())

	err := h.DB.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		// Lock the invoice row first. Pay takes the same lock, so void and pay
		// are serialized and one of them always sees the other's result.
		var inv models.Invoice
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND business_id = ?", id, bid).
			Take(&inv).Error; err != nil {
			return err
		}
		if !CanTransition(inv.Status, models.InvoiceVoid) {
			return &stateError{current: inv.Status}
		}

		// Never void while a payment may still succeed at the PSP.
		var inflight int64
		if err := tx.Table("payment_attempts").
			Where("invoice_id = ? AND status IN ?", id, []string{"pending", "unknown"}).
			Count(&inflight).Error; err != nil {
			return err
		}
		if inflight > 0 {
			return errPaymentInFlight
		}

		res := tx.Model(&models.Invoice{}).
			Where("id = ? AND status = ?", id, models.InvoiceOpen).
			Updates(map[string]any{"status": models.InvoiceVoid, "updated_at": time.Now()})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errors.New("void update affected no rows")
		}
		return nil
	})

	var se *stateError
	switch {
	case err == nil:
	case errors.Is(err, gorm.ErrRecordNotFound):
		httpx.NotFound(w, "Invoice not found")
		return
	case errors.As(err, &se):
		httpx.Conflict(w, "invalid_state_transition",
			"Cannot void an invoice that is "+se.current,
			map[string]any{"current_status": se.current})
		return
	case errors.Is(err, errPaymentInFlight):
		httpx.Conflict(w, "payment_in_progress", "A payment for this invoice is still in progress", nil)
		return
	default:
		httpx.Internal(w, err)
		return
	}

	inv, err := h.load(r.Context(), id, bid)
	if err != nil {
		httpx.Internal(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, inv)
}
