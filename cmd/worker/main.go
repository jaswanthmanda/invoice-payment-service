// Worker runs the background jobs: today the webhook dispatcher, and the
// payment reconciler in the next step. It is a separate process from the API,
// so a slow customer endpoint can never affect API latency.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"invoice-service/internal/config"
	"invoice-service/internal/db"
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

	// NEXT STEP: the payment reconciler starts here as a second goroutine.
	webhooks.NewDispatcher(gdb).Run(ctx) // blocks until shutdown
}

func fatal(msg string, err error) {
	slog.Error(msg, "err", err)
	os.Exit(1)
}
