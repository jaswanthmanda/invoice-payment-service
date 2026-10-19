// Package auth implements API key auth.
//
// Key format: sk_live_<prefix>_<secret>
//   - prefix: 8 hex chars, stored in plaintext, used for lookup
//   - secret: 32 random bytes (hex), never stored
//
// Only SHA-256(full key) is stored. A fast hash is fine here because the key is
// 256 bits of CSPRNG output, so it cannot be brute-forced like a password.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"invoice-service/internal/httpx"
	"invoice-service/internal/models"
)

type ctxKey struct{}

// GenerateKey returns the full key (show once), its prefix and its hash.
func GenerateKey() (full, prefix, hash string, err error) {
	p := make([]byte, 4)
	s := make([]byte, 32)
	if _, err = rand.Read(p); err != nil {
		return
	}
	if _, err = rand.Read(s); err != nil {
		return
	}
	prefix = hex.EncodeToString(p)
	full = fmt.Sprintf("sk_live_%s_%s", prefix, hex.EncodeToString(s))
	hash = HashKey(full)
	return
}

func HashKey(full string) string {
	sum := sha256.Sum256([]byte(full))
	return hex.EncodeToString(sum[:])
}

func parsePrefix(key string) (string, bool) {
	parts := strings.Split(key, "_")
	if len(parts) != 4 || parts[0] != "sk" || parts[1] != "live" || parts[2] == "" {
		return "", false
	}
	return parts[2], true
}

func bearerToken(r *http.Request) (string, bool) {
	const p = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) {
		return "", false
	}
	return strings.TrimSpace(h[len(p):]), true
}

// BusinessID returns the authenticated business for this request.
// Every query in the API must filter by it.
func BusinessID(ctx context.Context) uuid.UUID {
	id, _ := ctx.Value(ctxKey{}).(uuid.UUID)
	return id
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Missing, invalid, or revoked API key", nil)
}

// Middleware authenticates the request. Every failure returns the same generic
// 401, so a caller can't tell which check failed.
func Middleware(gdb *gorm.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, ok := bearerToken(r)
			if !ok {
				unauthorized(w)
				return
			}
			prefix, ok := parsePrefix(key)
			if !ok {
				unauthorized(w)
				return
			}

			var k models.APIKey
			err := gdb.WithContext(r.Context()).Where("prefix = ?", prefix).Take(&k).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				unauthorized(w)
				return
			}
			if err != nil {
				slog.Error("auth lookup failed", "err", err)
				httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "Internal server error", nil)
				return
			}

			if subtle.ConstantTimeCompare([]byte(HashKey(key)), []byte(k.KeyHash)) != 1 || k.RevokedAt != nil {
				unauthorized(w)
				return
			}

			// Best-effort, at most once a minute per key, to avoid a write per request.
			if k.LastUsedAt == nil || time.Since(*k.LastUsedAt) > time.Minute {
				_ = gdb.Model(&models.APIKey{}).Where("id = ?", k.ID).Update("last_used_at", time.Now()).Error
			}

			ctx := context.WithValue(r.Context(), ctxKey{}, k.BusinessID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
