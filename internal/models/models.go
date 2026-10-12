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
