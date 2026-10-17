package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestErrorPatternsGroupsRealPaymentAPIFailures(t *testing.T) {
	var lines []LogLine
	for i := 0; i < 30; i++ {
		lines = append(lines, LogLine{Pod: fmt.Sprintf("payment-api-%d", i%2), Time: fmt.Sprintf("2026-10-17T14:%02d:00Z", i),
			Text: fmt.Sprintf(`{"level":"ERROR","msg":"database operation failed","op":"create payment","request_id":"%016x","error":"failed to connect to %s: hostname resolving error: lookup wrong-host on 172.20.0.10:53: no such host"}`, i, "`user=payments database=payments`")})
	}
	for i := 0; i < 5; i++ {
		lines = append(lines, LogLine{Pod: "payment-api-0", Time: fmt.Sprintf("2026-10-17T15:%02d:00Z", i),
			Text: `{"level":"ERROR","msg":"database operation failed","op":"get payment","request_id":"abc","error":"failed SASL auth: FATAL: password authentication failed for user payments (SQLSTATE 28P01)"}`})
	}
	lines = append(lines,
		LogLine{Pod: "payment-api-0", Text: `{"level":"INFO","msg":"request","status":201}`},
		LogLine{Pod: "payment-api-0", Text: "plain text line with an Error inside"},
	)

	rep := errorPatterns(lines, 10)
	if rep.LinesScanned != 37 || rep.ErrorLines != 36 {
		t.Fatalf("scanned=%d error=%d", rep.LinesScanned, rep.ErrorLines)
	}
	if len(rep.Patterns) != 3 {
		t.Fatalf("got %d patterns: %+v", len(rep.Patterns), rep.Patterns)
	}
	top := rep.Patterns[0]
	if top.Count != 30 {
		t.Fatalf("top count %d, want 30", top.Count)
	}
	// Per-request values are masked, but the host that explains the incident is kept.
	if !strings.Contains(top.Template, "lookup wrong-host on <ip>") {
		t.Errorf("template lost the host: %q", top.Template)
	}
	if top.FirstSeen != "2026-10-17T14:00:00Z" || top.LastSeen != "2026-10-17T14:29:00Z" {
		t.Errorf("first/last = %s/%s", top.FirstSeen, top.LastSeen)
	}
	if len(top.Pods) != 2 || len(top.RequestIDs) != 3 {
		t.Errorf("pods=%v request_ids=%v", top.Pods, top.RequestIDs)
	}
	if rep.Patterns[1].Count != 5 || !strings.Contains(rep.Patterns[1].Template, "password authentication failed") {
		t.Errorf("second pattern %+v", rep.Patterns[1])
	}

	if capped := errorPatterns(lines, 1); !capped.Truncated || len(capped.Patterns) != 1 {
		t.Errorf("expected truncation to 1 pattern")
	}
}

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"postgres://payments:s3cr3t@postgres:5432/payments": "postgres://payments:[REDACTED]@postgres:5432/payments",
		`password="hunter2" next`:                           `password="[REDACTED]" next`,
		"Authorization: Bearer abcdefghijklmnopqrstuvwxyz":  "Authorization: Bearer [REDACTED]",
		"key AKIAABCDEFGHIJKLMNOP used":                     "key [REDACTED_AWS_KEY] used",
		// Must not mangle the error the agent needs to read.
		"failed SASL auth: FATAL: password authentication failed": "failed SASL auth: FATAL: password authentication failed",
	}
	for in, want := range cases {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
}
