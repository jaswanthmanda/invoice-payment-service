// Worker runs the background jobs: the webhook dispatcher and the payment
// reconciler. It is a separate process from the API, so a slow customer
// endpoint or a slow PSP can never affect API latency.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"invoice-service/internal/config"
	"invoice-service/internal/db"
	"invoice-service/internal/payments"
	"invoice-service/internal/psp"
	"invoice-service/internal/webhooks"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, err := config.Load()
	if err != nil {
		fatal("config", err)
	}
	// Safe to run alongside the API: golang-migrate takes a lock.
	if err := db.Migrate(cfg.DatabaseURL); err != nil {
		fatal("migrate", err)
	}
	gdb, err := db.Open(cfg.DatabaseURL)
	if err != nil {
		fatal("db open", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rec := &payments.Reconciler{DB: gdb, PSP: psp.New(cfg.PSPURL, 5*time.Second)}
	go rec.Run(ctx)

	webhooks.NewDispatcher(gdb).Run(ctx) // blocks until shutdown
}

func fatal(msg string, err error) {
	slog.Error(msg, "err", err)
	os.Exit(1)
}
