package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- fakes --------------------------------------------------------------

type fakeKube struct{}

func (fakeKube) health(context.Context, string) (*ServiceHealth, error) {
	return &ServiceHealth{Service: "payment-api", Deployment: &DeploymentStatus{Desired: 2, Ready: 2}}, nil
}
func (fakeKube) rollouts(context.Context, string, int) ([]Revision, error) {
	return []Revision{{Revision: 2, Changes: []string{`env DATABASE_HOST: "postgres" -> "wrong-host"`}}}, nil
}
func (fakeKube) logs(context.Context, string, time.Duration, int64) ([]LogLine, error) {
	return []LogLine{{Pod: "p", Time: "t", Text: `{"level":"ERROR","msg":"database operation failed","request_id":"r1","error":"lookup wrong-host: no such host"}`}}, nil
}

type fakeProm struct{}

func (fakeProm) summary(_ context.Context, name, _, _ string) (*MetricSummary, error) {
	v := 1.0
	return &MetricSummary{Query: name, Current: &v}, nil
}
func (fakeProm) alerts(context.Context, string) ([]FiringAlert, error) {
	return []FiringAlert{{Name: "PaymentApiHighErrorRate", State: "firing"}}, nil
}

type fakeGit struct{ prs []PullRequestInput }

func (f *fakeGit) recentCommits(context.Context, []string, int) ([]Commit, error) {
	return []Commit{{SHA: "abc123", Message: "set DATABASE_HOST"}}, nil
}
func (f *fakeGit) readFile(_ context.Context, path string) (string, error) {
	if path == "docs/runbooks/payment-api.md" {
		return "# Runbook\n## Known causes\n", nil
	}
	return "content of " + path, nil
}
func (f *fakeGit) pullRequest(_ context.Context, n int) (*PullRequestStatus, error) {
	return &PullRequestStatus{Number: n, Branch: "agent/fix-db-host", State: "closed", Merged: true, MergedBy: "CalebAche"}, nil
}
func (f *fakeGit) createPullRequest(_ context.Context, in PullRequestInput) (*PullRequestResult, error) {
	f.prs = append(f.prs, in)
	return &PullRequestResult{Number: 7, URL: "https://github.com/x/pull/7", Branch: in.Branch}, nil
}

// --- harness ------------------------------------------------------------

// tokens maps bearer tokens to identities, standing in for TokenReview.
var tokens = map[string]string{
	"agent-token":    agent,
	"intruder-token": "system:serviceaccount:default:default",
}

func fakeVerifier(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	user, ok := tokens[token]
	if !ok {
		return nil, auth.ErrInvalidToken
	}
	return &auth.TokenInfo{UserID: user, Expiration: time.Now().Add(time.Hour)}, nil
}

type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

func startServer(t *testing.T) (*httptest.Server, *fakeGit) {
	t.Helper()
	p, err := newPolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	git := &fakeGit{}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv := newMCPServer(&deps{kube: fakeKube{}, prom: fakeProm{}, git: git, environment: "dev", logger: logger}, p, newMetrics(), logger)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(auth.RequireBearerToken(fakeVerifier, nil)(h))
	t.Cleanup(ts.Close)
	return ts, git
}

func connect(t *testing.T, url, token string) (*mcp.ClientSession, error) {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	return c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: url, HTTPClient: &http.Client{Transport: bearer(token)},
	}, nil)
}

func callJSON(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()
	r, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	b, _ := json.Marshal(r.StructuredContent)
	return r, string(b)
}

// --- tests --------------------------------------------------------------

func TestAgentSessionEndToEnd(t *testing.T) {
	ts, git := startServer(t)
	s, err := connect(t, ts.URL, "agent-token")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	tools, err := s.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 10 {
		t.Fatalf("tools: %v %v", lenTools(tools), err)
	}

	r, body := callJSON(t, s, "get_incident_context", map[string]any{"service": "payment-api"})
	if r.IsError || !strings.Contains(body, "wrong-host") || !strings.Contains(body, "platform://services/payment-api/runbook") {
		t.Fatalf("incident context: %s", body)
	}

	r, body = callJSON(t, s, "patch_deployment", map[string]any{"service": "payment-api", "patch": "{}"})
	if !r.IsError || !strings.Contains(body, `"required_path":"create_pull_request"`) || !strings.Contains(body, "prohibited") {
		t.Fatalf("patch must be refused with a required path: %s", body)
	}

	r, body = callJSON(t, s, "get_service_health", map[string]any{"service": "kube-system"})
	if !r.IsError || !strings.Contains(body, "not in scope") {
		t.Fatalf("out-of-scope service must be refused: %s", body)
	}

	r, body = callJSON(t, s, "create_pull_request", map[string]any{
		"branch": "agent/x", "title": "t", "body": "b",
		"files": []map[string]string{{"path": "gitops/workloads/dev/payment-api/s.yaml", "content": "kind: Secret\n"}},
	})
	if !r.IsError || !strings.Contains(body, "Secret") {
		t.Fatalf("secret PR must be refused: %s", body)
	}
	if len(git.prs) != 0 {
		t.Fatal("a refused PR reached GitHub")
	}

	r, body = callJSON(t, s, "create_pull_request", map[string]any{
		"branch": "agent/fix-db-host", "title": "fix(payment-api): restore DATABASE_HOST", "body": "evidence",
		"files": []map[string]string{{"path": "gitops/workloads/dev/payment-api/deployment.yaml", "content": "kind: Deployment\n"}},
	})
	if r.IsError || !strings.Contains(body, "pull/7") || len(git.prs) != 1 {
		t.Fatalf("valid PR should be created: %s", body)
	}

	r, body = callJSON(t, s, "get_pull_request", map[string]any{"number": 7})
	if r.IsError || !strings.Contains(body, `"merged":true`) {
		t.Fatalf("get_pull_request: %s", body)
	}

	rr, err := s.ReadResource(ctx, &mcp.ReadResourceParams{URI: "platform://services/payment-api/runbook"})
	if err != nil || !strings.Contains(rr.Contents[0].Text, "Known causes") {
		t.Fatalf("runbook resource: %v", err)
	}
	// The agent tells a policy denial from other failures by this text.
	if _, err := s.ReadResource(ctx, &mcp.ReadResourceParams{URI: "file:///etc/passwd"}); err == nil || !strings.Contains(err.Error(), "denied by policy") {
		t.Fatalf("arbitrary resource must be refused by policy: %v", err)
	}

	gp, err := s.GetPrompt(ctx, &mcp.GetPromptParams{Name: "investigate_incident",
		Arguments: map[string]string{"service": "payment-api", "alert": "PaymentApiHighErrorRate"}})
	if err != nil {
		t.Fatal(err)
	}
	text := gp.Messages[0].Content.(*mcp.TextContent).Text
	if !strings.Contains(text, "`PaymentApiHighErrorRate` is firing for `payment-api`") {
		t.Fatalf("prompt not rendered: %q", text[:120])
	}
	if _, err := s.GetPrompt(ctx, &mcp.GetPromptParams{Name: "investigate_incident", Arguments: map[string]string{"service": "payment-api"}}); err == nil {
		t.Fatal("missing required prompt argument must fail")
	}
}

func TestUnauthorisedCallers(t *testing.T) {
	ts, _ := startServer(t)

	// No or bad token: rejected at the HTTP layer.
	if _, err := connect(t, ts.URL, "forged-token"); err == nil {
		t.Fatal("bad token must not connect")
	}

	// Valid token, unauthorised identity: authenticated but denied by policy.
	s, err := connect(t, ts.URL, "intruder-token")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.ListTools(context.Background(), nil); err == nil || !errors.Is(err, err) || !strings.Contains(err.Error(), "not authorised") {
		t.Fatalf("intruder listing must be refused, got %v", err)
	}
	r, body := callJSON(t, s, "get_service_health", map[string]any{"service": "payment-api"})
	if !r.IsError || !strings.Contains(body, "not authorised") {
		t.Fatalf("intruder tool call must be refused: %s", body)
	}
}
