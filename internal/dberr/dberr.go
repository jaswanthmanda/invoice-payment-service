// Package dberr classifies Postgres errors so handlers can map them to API errors.
package dberr

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// UniqueViolation reports whether err is a Postgres unique violation (23505)
// and returns the name of the constraint or index that was violated.
func UniqueViolation(err error) (constraint string, ok bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName, true
	}
	return "", false
}
