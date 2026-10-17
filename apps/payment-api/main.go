// Command payment-api is the demo workload for the agentic SRE PoC.
//
// It runs in one of two modes:
//
//	payment-api serve     HTTP API backed by Postgres (default)
//	payment-api loadgen   steady synthetic traffic against a running API
//
// The deliberate failure is a wrong DATABASE_HOST: the process stays up and
// ready, but every request that touches the database returns HTTP 500.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// version is set at build time with -ldflags "-X main.version=<git sha>".
var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "payment-api", "version", version)
	slog.SetDefault(logger)

	mode := "serve"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch mode {
	case "serve":
		err = serve(ctx, logger)
	case "loadgen":
		err = runLoadgen(ctx, logger, os.Args[2:])
	default:
		err = fmt.Errorf("unknown mode %q (want serve or loadgen)", mode)
	}
	if err != nil {
		logger.Error("exiting", "error", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, logger *slog.Logger) error {
	cfg, err := dbConfigFromEnv()
	if err != nil {
		return err
	}
	store, err := newPostgresStore(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer store.Close()

	metrics := newMetrics()
	api := &http.Server{
		Addr:              envOr("LISTEN_ADDR", ":8080"),
		Handler:           newServer(store, metrics, logger).routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	// Metrics on a separate port so they are never exposed through the
	// public Gateway route, which only targets the API port.
	metricsSrv := &http.Server{
		Addr:              envOr("METRICS_ADDR", ":9090"),
		Handler:           metrics.handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 2)
	for _, srv := range []*http.Server{api, metricsSrv} {
		go func() {
			logger.Info("listening", "addr", srv.Addr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}

	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return errors.Join(api.Shutdown(shutdownCtx), metricsSrv.Shutdown(shutdownCtx))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
