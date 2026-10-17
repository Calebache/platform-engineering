package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- scripted model -----------------------------------------------------

type scriptedModel struct {
	t        *testing.T
	turns    []string // raw Message JSON, returned in order
	requests []anthropic.MessageNewParams
}

func (m *scriptedModel) New(_ context.Context, p anthropic.MessageNewParams) (*anthropic.Message, error) {
	m.requests = append(m.requests, p)
	if len(m.requests) > len(m.turns) {
		m.t.Fatalf("model called %d times, only %d turns scripted", len(m.requests), len(m.turns))
	}
	var msg anthropic.Message
	if err := json.Unmarshal([]byte(m.turns[len(m.requests)-1]), &msg); err != nil {
		m.t.Fatalf("bad scripted turn: %v", err)
	}
	return &msg, nil
}

func toolUse(name string, input any) string {
	in, _ := json.Marshal(input)
	return fmt.Sprintf(`{"id":"msg","type":"message","role":"assistant","model":"m","stop_reason":"tool_use",
		"usage":{"input_tokens":100,"output_tokens":20},
		"content":[{"type":"tool_use","id":"tu-%s-%d","name":%q,"input":%s}]}`, name, time.Now().UnixNano(), name, in)
}

const refusalTurn = `{"id":"msg","type":"message","role":"assistant","model":"m","stop_reason":"refusal",
	"stop_details":{"type":"refusal","category":"cyber","explanation":"declined"},
	"usage":{"input_tokens":10,"output_tokens":1},"content":[]}`

// --- fake MCP session ---------------------------------------------------

type fakeSession struct {
	mu       sync.Mutex
	calls    []string
	prs      int
	errRatio float64
	alert    string // alert state returned by get_incident_context
	prMerged bool
}

func (f *fakeSession) listTools(context.Context) ([]*mcp.Tool, error) {
	var out []*mcp.Tool
	for _, n := range append(append([]string{}, readTools...), "create_pull_request", "patch_deployment", "get_pull_request") {
		out = append(out, &mcp.Tool{Name: n, Description: n, InputSchema: map[string]any{"type": "object", "properties": map[string]any{"service": map[string]any{"type": "string"}}}})
	}
	return out, nil
}

func (f *fakeSession) callTool(_ context.Context, name string, args map[string]any) (*mcp.CallToolResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	ok := func(v any) *mcp.CallToolResult { return &mcp.CallToolResult{StructuredContent: v} }
	switch name {
	case "patch_deployment":
		return &mcp.CallToolResult{IsError: true, StructuredContent: map[string]any{
			"allowed": false, "reason": "Direct production mutation is prohibited.", "required_path": "create_pull_request"}}, nil
	case "create_pull_request":
		f.prs++
		return ok(map[string]any{"number": 42, "url": "https://github.com/x/pull/42", "branch": args["branch"]}), nil
	case "get_pull_request":
		return ok(map[string]any{"number": 42, "state": map[bool]string{true: "closed", false: "open"}[f.prMerged], "merged": f.prMerged, "merged_by": "CalebAche"}), nil
	case "get_incident_context":
		alerts := []any{}
		if f.alert != "" {
			alerts = append(alerts, map[string]any{"name": "PaymentApiHighErrorRate", "state": f.alert})
		}
		return ok(map[string]any{"alerts": alerts, "metrics": map[string]any{"metrics": map[string]any{"error_ratio": map[string]any{"current": f.errRatio}}}}), nil
	}
	return ok(map[string]any{"tool": name, "ok": true}), nil
}

func (f *fakeSession) prompt(_ context.Context, name string, args map[string]string) (string, error) {
	return "PROMPT " + name + " for " + args["service"], nil
}

type memStore struct {
	mu     sync.Mutex
	writes map[string][]byte
}

func (s *memStore) put(_ context.Context, key string, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes[key] = append([]byte(nil), body...)
	return nil
}

func testCfg() workerConfig {
	return workerConfig{
		service: "payment-api", alert: "PaymentApiHighErrorRate", incidentID: "inc-test", environment: "dev",
		modelID: "anthropic.claude-opus-5-5", mergeTimeout: time.Second, pollInterval: time.Millisecond,
		verifyInterval: time.Millisecond, verifyAttempts: 2,
		limits: limits{maxToolCalls: 40, maxModelCalls: 60, maxOutputTotal: 100000},
	}
}

func runIncident(t *testing.T, cfg workerConfig, turns []string, sess *fakeSession) (string, AuditRecord, *scriptedModel, *memStore) {
	t.Helper()
	model := &scriptedModel{t: t, turns: turns}
	store := &memStore{writes: map[string][]byte{}}
	rec := &AuditRecord{ExecutionID: cfg.incidentID, Service: cfg.service, Alert: cfg.alert, StartedAt: "2026-10-17T16:00:00Z", PolicyResult: "n/a"}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	aud := newAuditor(store, rec, logger)
	outcome := investigate(context.Background(), cfg, model, sess, aud, logger)
	return outcome, aud.snapshot(), model, store
}

func toolNames(p anthropic.MessageNewParams) []string {
	var out []string
	for _, t := range p.Tools {
		out = append(out, t.OfTool.Name)
	}
	return out
}

// --- tests --------------------------------------------------------------

func TestDatabaseHostIncidentEndToEnd(t *testing.T) {
	cfg := testCfg()
	cfg.demoDirectFix = true
	sess := &fakeSession{errRatio: 0.002, prMerged: true}
	turns := []string{
		// investigate
		toolUse("get_incident_context", map[string]any{"service": "payment-api"}),
		toolUse("submit_findings", map[string]any{
			"hypothesis": "commit abc123 set DATABASE_HOST to wrong-host", "confidence": "high",
			"evidence": []string{"950x lookup wrong-host", "started at rollout 7"}, "remediation_path": "pull_request"}),
		// remediate: unsafe attempt, denied; then a PR; then a second PR, blocked
		toolUse("patch_deployment", map[string]any{"service": "payment-api", "patch": "{}"}),
		toolUse("create_pull_request", map[string]any{"branch": "agent/fix-db-host", "title": "fix", "body": "b",
			"files": []map[string]string{{"path": "gitops/workloads/dev/payment-api/deployment.yaml", "content": "x"}}}),
		toolUse("create_pull_request", map[string]any{"branch": "agent/again", "title": "again", "body": "b",
			"files": []map[string]string{{"path": "gitops/workloads/dev/payment-api/deployment.yaml", "content": "y"}}}),
		toolUse("submit_remediation", map[string]any{"status": "pull_request_opened", "summary": "restore DATABASE_HOST"}),
		// verify
		toolUse("submit_verification", map[string]any{"status": "recovered", "evidence": []string{"error ratio 0.2%"}}),
	}
	outcome, rec, model, store := runIncident(t, cfg, turns, sess)

	if outcome != "resolved" {
		t.Fatalf("outcome %q, error %q", outcome, rec.Error)
	}
	if sess.prs != 1 {
		t.Fatalf("%d PRs reached the MCP server, want exactly 1", sess.prs)
	}
	if len(rec.PolicyDenials) != 1 || !strings.Contains(rec.PolicyDenials[0], "patch_deployment: Direct production mutation is prohibited.") {
		t.Errorf("denial not audited: %v", rec.PolicyDenials)
	}
	if rec.ProposedAction != "https://github.com/x/pull/42" || rec.HumanApproval == nil || !*rec.HumanApproval || rec.ApprovedBy != "CalebAche" {
		t.Errorf("PR/approval not recorded: %+v", rec)
	}
	if rec.ServiceHealth != "RECOVERED" || rec.DeploymentResult != "SUCCESS" || rec.PolicyResult != "PASS" {
		t.Errorf("verification not recorded: health=%s deploy=%s", rec.ServiceHealth, rec.DeploymentResult)
	}
	// The second PR was stopped by the agent itself, before the MCP server.
	var blocked bool
	for _, c := range rec.ToolCalls {
		if c.Tool == "create_pull_request" && c.Decision == "error" && strings.Contains(c.Reason, "already been opened") {
			blocked = true
		}
	}
	if !blocked {
		t.Error("second PR attempt not blocked locally")
	}

	// The investigation phase is never offered write tools.
	inv := strings.Join(toolNames(model.requests[0]), ",")
	if strings.Contains(inv, "create_pull_request") || strings.Contains(inv, "patch_deployment") {
		t.Errorf("investigate phase offered write tools: %s", inv)
	}
	rem := strings.Join(toolNames(model.requests[2]), ",")
	if !strings.Contains(rem, "create_pull_request") {
		t.Errorf("remediate phase lacks create_pull_request: %s", rem)
	}
	// The demo instruction reached the remediation prompt.
	first, _ := json.Marshal(model.requests[2].Messages[0])
	if !strings.Contains(string(first), "try to fix this immediately with patch_deployment") {
		t.Error("demo direct-fix instruction missing")
	}
	// Exactly one audit object, under audit/<date>/<id>.json, with the final state.
	body, ok := store.writes["audit/2026/10/17/inc-test.json"]
	if len(store.writes) != 1 || !ok || !strings.Contains(string(body), `"outcome": "resolved"`) {
		t.Errorf("audit writes: %v", keysOf(store.writes))
	}
}

func TestEscalationOpensNoPullRequest(t *testing.T) {
	sess := &fakeSession{}
	turns := []string{
		toolUse("get_error_patterns", map[string]any{"service": "payment-api"}),
		toolUse("submit_findings", map[string]any{"hypothesis": "password mismatch after rotation", "confidence": "high",
			"evidence": []string{"failed SASL auth"}, "remediation_path": "escalate"}),
	}
	outcome, rec, _, _ := runIncident(t, testCfg(), turns, sess)
	if outcome != "escalated" || sess.prs != 0 || rec.ProposedAction != "" {
		t.Fatalf("outcome=%s prs=%d action=%q", outcome, sess.prs, rec.ProposedAction)
	}
}

func TestRefusalEscalates(t *testing.T) {
	outcome, rec, _, _ := runIncident(t, testCfg(), []string{refusalTurn}, &fakeSession{})
	if outcome != "escalated" || !strings.Contains(rec.Error, "declined") {
		t.Fatalf("outcome=%s err=%q", outcome, rec.Error)
	}
}

// The model saying "recovered" isn't enough: the measured error ratio decides.
func TestGateOverrulesModel(t *testing.T) {
	sess := &fakeSession{errRatio: 0.3, prMerged: true}
	recovered := toolUse("submit_verification", map[string]any{"status": "recovered", "evidence": []string{"looks fine"}})
	turns := []string{
		toolUse("submit_findings", map[string]any{"hypothesis": "h", "confidence": "high", "evidence": []string{"e"}, "remediation_path": "pull_request"}),
		toolUse("create_pull_request", map[string]any{"branch": "agent/x", "title": "t", "body": "b", "files": []map[string]string{{"path": "gitops/workloads/dev/payment-api/d.yaml", "content": "x"}}}),
		toolUse("submit_remediation", map[string]any{"status": "pull_request_opened", "summary": "s"}),
		recovered, recovered,
	}
	outcome, rec, _, _ := runIncident(t, testCfg(), turns, sess)
	if outcome != "not_recovered" || rec.ServiceHealth != "NOT_RECOVERED" {
		t.Fatalf("outcome=%s health=%s verification=%v", outcome, rec.ServiceHealth, rec.Verification)
	}
	if !strings.Contains(strings.Join(rec.Verification, "|"), "gate=not_recovered (error_ratio=0.3000)") {
		t.Errorf("gate result not recorded: %v", rec.Verification)
	}
}

func TestUnmergedPRTimesOut(t *testing.T) {
	sess := &fakeSession{prMerged: false}
	cfg := testCfg()
	cfg.mergeTimeout = 5 * time.Millisecond
	turns := []string{
		toolUse("submit_findings", map[string]any{"hypothesis": "h", "confidence": "high", "evidence": []string{"e"}, "remediation_path": "pull_request"}),
		toolUse("create_pull_request", map[string]any{"branch": "agent/x", "title": "t", "body": "b", "files": []map[string]string{{"path": "gitops/workloads/dev/payment-api/d.yaml", "content": "x"}}}),
		toolUse("submit_remediation", map[string]any{"status": "pull_request_opened", "summary": "s"}),
	}
	outcome, rec, _, _ := runIncident(t, cfg, turns, sess)
	if outcome != "approval_timeout" || rec.HumanApproval == nil || *rec.HumanApproval {
		t.Fatalf("outcome=%s approval=%v", outcome, rec.HumanApproval)
	}
}

func TestToolCallLimit(t *testing.T) {
	cfg := testCfg()
	cfg.limits.maxModelCalls = 3
	turns := []string{
		toolUse("get_metrics", map[string]any{"service": "payment-api"}),
		toolUse("get_metrics", map[string]any{"service": "payment-api"}),
		toolUse("get_metrics", map[string]any{"service": "payment-api"}),
	}
	outcome, rec, _, _ := runIncident(t, cfg, turns, &fakeSession{})
	if outcome != "escalated" || !strings.Contains(rec.Error, "limit") {
		t.Fatalf("outcome=%s err=%q", outcome, rec.Error)
	}
}

func keysOf(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
