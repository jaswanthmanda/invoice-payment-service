// Seed creates a business and an API key (printed once), or adds another key to
// an existing business, which is the rotation path: add a new key, switch over,
// then revoke the old one.
//
//	go run ./cmd/seed -name "Acme"
//	go run ./cmd/seed -business-id <uuid>
package main

import (
	"flag"
	"fmt"
	"log"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"invoice-service/internal/auth"
	"invoice-service/internal/config"
	"invoice-service/internal/db"
	"invoice-service/internal/models"
)

func main() {
	name := flag.String("name", "Demo Business", "name for a new business")
	bizID := flag.String("business-id", "", "add a key to this existing business instead of creating one")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	if err := db.Migrate(cfg.DatabaseURL); err != nil {
		log.Fatal(err)
	}
	gdb, err := db.Open(cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}

	fullKey, prefix, hash, err := auth.GenerateKey()
	if err != nil {
		log.Fatal(err)
	}

	var businessID uuid.UUID
	err = gdb.Transaction(func(tx *gorm.DB) error {
		if *bizID != "" {
			id, err := uuid.Parse(*bizID)
			if err != nil {
				return err
			}
			var b models.Business
			if err := tx.Where("id = ?", id).Take(&b).Error; err != nil {
				return fmt.Errorf("business not found: %w", err)
			}
			businessID = b.ID
		} else {
			b := models.Business{ID: models.NewID(), Name: *name}
			if err := tx.Create(&b).Error; err != nil {
				return err
			}
			businessID = b.ID
		}
		return tx.Create(&models.APIKey{
			ID:         models.NewID(),
			BusinessID: businessID,
			Prefix:     prefix,
			KeyHash:    hash,
		}).Error
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("business_id: %s\napi_key:     %s\n\nStore this key now. It is not recoverable.\n", businessID, fullKey)
}
