// Command sre-agent investigates incidents on the agentic SRE platform.
//
// It runs in one of three modes:
//
//	sre-agent orchestrator   receives Alertmanager webhooks and launches one
//	                         Kubernetes Job per incident. Holds no model
//	                         access and no AWS role.
//	sre-agent investigate    the incident worker (run by a Job): investigates
//	                         through the MCP server with Claude on Bedrock,
//	                         proposes a fix as a PR or escalates, waits for
//	                         approval, verifies recovery, writes the audit record.
//	sre-agent whoami         proves the worker's identity and its limits
//	                         (doc.md §17): what it may call, and what it may not.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "sre-agent", "version", version)
	slog.SetDefault(logger)

	mode := ""
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch mode {
	case "orchestrator":
		err = runOrchestrator(ctx, logger)
	case "investigate":
		err = runInvestigate(ctx, logger, os.Args[2:])
	case "whoami":
		err = runWhoami(ctx, os.Args[2:])
	default:
		err = fmt.Errorf("usage: sre-agent orchestrator|investigate|whoami (got %q)", mode)
	}
	if err != nil {
		logger.Error("exiting", "error", err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func mustEnv(key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		return "", fmt.Errorf("%s must be set", key)
	}
	return v, nil
}
