package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"invoice-service/internal/auth"
	"invoice-service/internal/config"
	"invoice-service/internal/db"
	"invoice-service/internal/httpx"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, err := config.Load()
	if err != nil {
		fatal("config", err)
	}
	if err := db.Migrate(cfg.DatabaseURL); err != nil {
		fatal("migrate", err)
	}
	gdb, err := db.Open(cfg.DatabaseURL)
	if err != nil {
		fatal("db open", err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		fatal("db handle", err)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := sqlDB.PingContext(ctx); err != nil {
			httpx.WriteError(w, http.StatusServiceUnavailable, "unhealthy", "Database unreachable", nil)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	authed := auth.Middleware(gdb)

	// Temporary smoke test for auth. Real routes replace it in the next steps.
	mux.Handle("GET /v1/me", authed(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"business_id": auth.BusinessID(r.Context())})
	})))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "Route not found", nil)
	})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpx.Logging(httpx.Recover(mux)),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("api listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal("server", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	slog.Info("api stopped")
}

func fatal(msg string, err error) {
	slog.Error(msg, "err", err)
	os.Exit(1)
}
