package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// runLoadgen sends a steady trickle of create-then-read requests so the API
// always has traffic for its error-rate alert to measure.
func runLoadgen(ctx context.Context, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	target := fs.String("target", envOr("LOADGEN_TARGET", "http://localhost:8080"), "base URL of payment-api")
	interval := fs.Duration("interval", 500*time.Millisecond, "time between iterations")
	if err := fs.Parse(args); err != nil {
		return err
	}
	base := strings.TrimRight(*target, "/")
	client := &http.Client{Timeout: 5 * time.Second}

	logger.Info("loadgen starting", "target", base, "interval", interval.String())
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	report := time.NewTicker(30 * time.Second)
	defer report.Stop()

	counts := map[string]int{}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-report.C:
			logger.Info("loadgen summary (last 30s)", "status_counts", counts)
			counts = map[string]int{}
		case <-ticker.C:
			id, status := post(ctx, client, base)
			counts[status]++
			if id != "" {
				counts[get(ctx, client, base, id)]++
			}
		}
	}
}

func post(ctx context.Context, c *http.Client, base string) (id, status string) {
	body, _ := json.Marshal(createPaymentRequest{AmountCents: 1999, Currency: "CAD"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/payments", bytes.NewReader(body))
	if err != nil {
		return "", "error"
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return "", "error"
	}
	defer resp.Body.Close()
	var p Payment
	if resp.StatusCode == http.StatusCreated {
		_ = json.NewDecoder(resp.Body).Decode(&p)
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	return p.ID, fmt.Sprint(resp.StatusCode)
}

func get(ctx context.Context, c *http.Client, base, id string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/payments/"+id, nil)
	if err != nil {
		return "error"
	}
	resp, err := c.Do(req)
	if err != nil {
		return "error"
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return fmt.Sprint(resp.StatusCode)
}
