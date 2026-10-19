package customers

import (
	"errors"
	"net/http"
	"net/mail"
	"strings"

	"gorm.io/gorm"

	"invoice-service/internal/auth"
	"invoice-service/internal/dberr"
	"invoice-service/internal/httpx"
	"invoice-service/internal/models"
)

type Handler struct {
	DB *gorm.DB
}

type createRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Create handles POST /v1/customers.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}

	name := strings.TrimSpace(req.Name)
	email := strings.ToLower(strings.TrimSpace(req.Email)) // normalize so A@x.com == a@x.com

	problems := map[string]any{}
	if name == "" || len(name) > 200 {
		problems["name"] = "must be 1 to 200 characters"
	}
	if !validEmail(email) {
		problems["email"] = "must be a valid email address"
	}
	if len(problems) > 0 {
		httpx.Unprocessable(w, "Validation failed", problems)
		return
	}

	c := models.Customer{
		ID:         models.NewID(),
		BusinessID: auth.BusinessID(r.Context()),
		Name:       name,
		Email:      email,
	}
	if err := h.DB.WithContext(r.Context()).Create(&c).Error; err != nil {
		// UNIQUE (business_id, email) is the only unique rule besides the PK.
		if _, ok := dberr.UniqueViolation(err); ok {
			httpx.Conflict(w, "customer_exists", "A customer with this email already exists", nil)
			return
		}
		httpx.Internal(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, c)
}

// Get handles GET /v1/customers/{id}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathID(r)
	if !ok {
		httpx.NotFound(w, "Customer not found")
		return
	}

	var c models.Customer
	err := h.DB.WithContext(r.Context()).
		Where("id = ? AND business_id = ?", id, auth.BusinessID(r.Context())).
		Take(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		httpx.NotFound(w, "Customer not found") // same answer for "other tenant's"
		return
	}
	if err != nil {
		httpx.Internal(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, c)
}

// List handles GET /v1/customers?limit=&starting_after=.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	page, ok := httpx.ParsePage(w, r)
	if !ok {
		return
	}

	q := h.DB.WithContext(r.Context()).
		Where("business_id = ?", auth.BusinessID(r.Context()))
	if page.After != nil {
		q = q.Where("id < ?", *page.After)
	}

	customers := make([]models.Customer, 0, page.Limit+1)
	// Fetch one extra row to know whether another page exists.
	if err := q.Order("id DESC").Limit(page.Limit + 1).Find(&customers).Error; err != nil {
		httpx.Internal(w, err)
		return
	}

	hasMore := len(customers) > page.Limit
	if hasMore {
		customers = customers[:page.Limit]
	}
	httpx.WriteList(w, customers, hasMore)
}

func validEmail(s string) bool {
	if s == "" || len(s) > 320 {
		return false
	}
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s
}
