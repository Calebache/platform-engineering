// Command mcp-server is the governed interface between the SRE agent and the
// platform: a Model Context Protocol server exposing investigation tools,
// documentation resources and workflow prompts.
//
// Every request is authenticated (Kubernetes TokenReview) and authorised
// (embedded OPA policy, policy/mcp.rego) in one middleware before any
// handler runs, and every decision is logged.
//
//	mcp-server serve   run the server (default)
//	mcp-server smoke   call the server as a client, for end-to-end checks
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

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "mcp-server", "version", version)
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
	case "smoke":
		err = runSmoke(ctx, os.Args[2:])
	default:
		err = fmt.Errorf("unknown mode %q (want serve or smoke)", mode)
	}
	if err != nil {
		logger.Error("exiting", "error", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, logger *slog.Logger) error {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("in-cluster config: %w", err)
	}
	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return err
	}
	gh, err := newGitHubFromEnv(ctx)
	if err != nil {
		return err
	}
	policy, err := newPolicy(ctx)
	if err != nil {
		return err
	}

	deps := &deps{
		kube:   &kubeBackend{client: kube, dynamic: dyn},
		prom:   &promBackend{baseURL: envOr("PROMETHEUS_URL", "http://kube-prometheus-stack-prometheus.monitoring.svc:9090"), http: &http.Client{Timeout: 10 * time.Second}},
		git:    gh,
		logger: logger,
	}
	metrics := newMetrics()
	srv := newMCPServer(deps, policy, metrics, logger)

	verifier := newTokenReviewVerifier(kube, envOr("TOKEN_AUDIENCE", "mcp-server"))
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Logger: logger})

	mux := http.NewServeMux()
	mux.Handle("/mcp", auth.RequireBearerToken(verifier.verify, nil)(mcpHandler))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })

	api := &http.Server{Addr: envOr("LISTEN_ADDR", ":8080"), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	metricsSrv := &http.Server{Addr: envOr("METRICS_ADDR", ":9090"), Handler: metrics.handler(), ReadHeaderTimeout: 5 * time.Second}

	errCh := make(chan error, 2)
	for _, s := range []*http.Server{api, metricsSrv} {
		go func() {
			logger.Info("listening", "addr", s.Addr)
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}
	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return errors.Join(api.Shutdown(shutdownCtx), metricsSrv.Shutdown(shutdownCtx))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
