// Package models holds plain GORM structs. No gorm.Model: IDs are UUIDv7
// generated in code, and the schema lives in the SQL migrations.
package models

import (
	"time"

	"github.com/google/uuid"
)

// NewID returns a time-ordered UUIDv7.
func NewID() uuid.UUID {
	return uuid.Must(uuid.NewV7())
}

// Invoice states. Transition rules live in one place (the invoices package).
const (
	InvoiceOpen = "open"
	InvoicePaid = "paid"
	InvoiceVoid = "void"
)

type Business struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey"`
	Name      string
	CreatedAt time.Time
}

func (Business) TableName() string { return "businesses" }

type APIKey struct {
	ID         uuid.UUID `gorm:"type:uuid;primaryKey"`
	BusinessID uuid.UUID `gorm:"type:uuid"`
	Prefix     string
	KeyHash    string
	RevokedAt  *time.Time
	LastUsedAt *time.Time
	CreatedAt  time.Time
}

func (APIKey) TableName() string { return "api_keys" }

type Customer struct {
	ID         uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	BusinessID uuid.UUID `gorm:"type:uuid" json:"-"`
	Name       string    `json:"name"`
	Email      string    `json:"email"`
	CreatedAt  time.Time `json:"created_at"`
}

type Invoice struct {
	ID         uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	BusinessID uuid.UUID  `gorm:"type:uuid" json:"-"`
	CustomerID uuid.UUID  `gorm:"type:uuid" json:"customer_id"`
	Status     string     `json:"status"`
	TotalCents int64      `json:"total_cents"`
	Currency   string     `json:"currency"`
	DueDate    Date       `json:"due_date"`
	PaidAt     *time.Time `json:"paid_at"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`

	LineItems []InvoiceLineItem `gorm:"foreignKey:InvoiceID" json:"line_items"`
}

type InvoiceLineItem struct {
	ID              uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	InvoiceID       uuid.UUID `gorm:"type:uuid" json:"-"`
	Description     string    `json:"description"`
	Quantity        int       `json:"quantity"`
	UnitAmountCents int64     `json:"unit_amount_cents"`
}
