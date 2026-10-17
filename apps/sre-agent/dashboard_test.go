package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeReader struct {
	mu   sync.Mutex
	objs map[string]auditObject
	body map[string][]byte
	gets int
}

func (f *fakeReader) put(t *testing.T, rec AuditRecord, etag string, modified time.Time) {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.objs == nil {
		f.objs, f.body = map[string]auditObject{}, map[string][]byte{}
	}
	key := "audit/2026/10/17/" + rec.ExecutionID + ".json"
	f.objs[key] = auditObject{Key: key, ETag: etag, Modified: modified}
	f.body[key] = b
}

func (f *fakeReader) list(context.Context) ([]auditObject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []auditObject
	for _, o := range f.objs {
		out = append(out, o)
	}
	return out, nil
}

func (f *fakeReader) get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	return f.body[key], nil
}

var t0 = time.Date(2026, 10, 17, 22, 0, 0, 0, time.UTC)

func sampleRecords() []AuditRecord {
	yes := true
	return []AuditRecord{
		{
			ExecutionID: "inc-1-resolved", Service: "payment-api", Alert: "PaymentApiHighErrorRate",
			StartedAt: t0.Format(time.RFC3339), FinishedAt: t0.Add(9 * time.Minute).Format(time.RFC3339),
			Outcome: "resolved", Hypothesis: "Commit abc set DATABASE_HOST to a host that doesn't exist.",
			ProposedAction: "https://github.com/Calebache/platform-engineering/pull/1", HumanApproval: &yes, ApprovedBy: "Calebache",
			ToolCalls: []ToolCall{
				{Phase: "investigate", Tool: "read_resource", Decision: "allowed", Args: `{"uri":"platform://services/payment-api/runbook"}`},
				{Phase: "remediate", Tool: "patch_deployment", Decision: "denied", Reason: "direct mutation; use create_pull_request"},
				{Phase: "remediate", Tool: "create_pull_request", Decision: "allowed"},
			},
		},
		{
			ExecutionID: "inc-2-escalated", Service: "payment-api", Alert: "PaymentApiHighErrorRate",
			StartedAt: t0.Add(time.Hour).Format(time.RFC3339), FinishedAt: t0.Add(time.Hour + 2*time.Minute).Format(time.RFC3339),
			Outcome: "escalated", Hypothesis: `Credentials rotated <script>alert(1)</script>`,
			Recommendation: "Restore the previous secret version, then force-sync the ExternalSecret.",
		},
		{
			ExecutionID: "inc-3-running", Service: "payment-api", Alert: "PaymentApiDown",
			StartedAt: t0.Add(2 * time.Hour).Format(time.RFC3339), Phase: "investigate",
		},
	}
}

// A record that never finished, long after a Job would have been killed, is
// stopped, not running; whoami's probes under audit/ aren't incidents.
func TestDashboardStoppedAndProbes(t *testing.T) {
	srv, r := newTestDashboard(t)
	r.put(t, AuditRecord{ExecutionID: "inc-0-orphan", Service: "payment-api", Alert: "PaymentApiHighErrorRate",
		StartedAt: t0.Add(-time.Hour).Format(time.RFC3339)}, "e1", t0.Add(-time.Hour))
	r.mu.Lock()
	r.objs["audit/whoami/20261017T220316Z.json"] = auditObject{Key: "audit/whoami/20261017T220316Z.json", ETag: "w", Modified: t0}
	r.body["audit/whoami/20261017T220316Z.json"] = []byte(`{"probe":"whoami"}`)
	r.objs["audit/2026/10/17/junk.json"] = auditObject{Key: "audit/2026/10/17/junk.json", ETag: "j", Modified: t0}
	r.body["audit/2026/10/17/junk.json"] = []byte(`{"probe":"no execution id"}`)
	r.mu.Unlock()
	_, body := get(t, srv.routes(), "/")
	if !strings.Contains(body, ">stopped<") || !strings.Contains(body, "4</b><span>Incidents") {
		t.Fatalf("orphan not shown as stopped, or probes counted:\n%s", body)
	}
}

func newTestDashboard(t *testing.T) (*dashboardServer, *fakeReader) {
	t.Helper()
	r := &fakeReader{}
	for i, rec := range sampleRecords() {
		r.put(t, rec, "e1", t0.Add(time.Duration(i)*time.Hour))
	}
	store := newIncidentStore(r, time.Minute, 200, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := func() time.Time { return t0.Add(3 * time.Hour) }
	store.now = now
	srv, err := newDashboardServer(store, "dev")
	if err != nil {
		t.Fatal(err)
	}
	srv.now = now
	return srv, r
}

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	return rr.Code, rr.Body.String()
}

func TestDashboardIndex(t *testing.T) {
	srv, _ := newTestDashboard(t)
	code, body := get(t, srv.routes(), "/")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{
		"Restore the previous secret version", // the escalation's recommendation
		"in progress",                         // a record still being written
		"1 docs read", "1 denied by policy",
		`href="https://github.com/Calebache/platform-engineering/pull/1"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index missing %q", want)
		}
	}
	// Newest first.
	if strings.Index(body, "inc-3-running") > strings.Index(body, "inc-1-resolved") {
		t.Error("incidents not newest first")
	}
	// Agent-written text comes from logs and commits: it must be escaped.
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("hypothesis rendered unescaped")
	}
}

func TestInlineMarkdownEscapesFirst(t *testing.T) {
	got := string(inlineMarkdown("Set `DATABASE_HOST` to **x** <img src=x onerror=alert(1)> `<b>`"))
	want := "Set <code>DATABASE_HOST</code> to <strong>x</strong> &lt;img src=x onerror=alert(1)&gt; <code>&lt;b&gt;</code>"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestDashboardSummary(t *testing.T) {
	var vs []incidentView
	for _, r := range sampleRecords() {
		vs = append(vs, viewOf(r, t0.Add(3*time.Hour)))
	}
	s := summarize(vs)
	if s.Total != 3 || s.Resolved != 1 || s.NeedsHuman != 1 || s.Running != 1 || s.PRs != 1 || s.Denials != 1 {
		t.Fatalf("summary %+v", s)
	}
}

func TestDashboardIncidentPage(t *testing.T) {
	srv, _ := newTestDashboard(t)
	code, body := get(t, srv.routes(), "/incidents/inc-1-resolved")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{"patch_deployment", "direct mutation; use create_pull_request", "Yes by Calebache", "investigate", "remediate"} {
		if !strings.Contains(body, want) {
			t.Errorf("incident page missing %q", want)
		}
	}
	if code, _ := get(t, srv.routes(), "/incidents/nope"); code != http.StatusNotFound {
		t.Errorf("unknown incident: status %d", code)
	}
}

func TestDashboardRefetchesOnlyChangedRecords(t *testing.T) {
	srv, r := newTestDashboard(t)
	ctx := context.Background()
	if _, err := srv.store.incidents(ctx); err != nil {
		t.Fatal(err)
	}
	if r.gets != 3 {
		t.Fatalf("first load: %d gets", r.gets)
	}
	// One record changes (new ETag); after the refresh interval only it is refetched.
	rec := sampleRecords()[2]
	rec.Outcome, rec.FinishedAt = "escalated", t0.Add(2*time.Hour+time.Minute).Format(time.RFC3339)
	r.put(t, rec, "e2", t0.Add(2*time.Hour+time.Minute))
	srv.store.now = func() time.Time { return t0.Add(4 * time.Hour) }
	recs, err := srv.store.incidents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.gets != 4 {
		t.Fatalf("refresh refetched %d records, want 1", r.gets-3)
	}
	if recs[0].ExecutionID != "inc-3-running" || recs[0].Outcome != "escalated" {
		t.Fatalf("updated record not served: %+v", recs[0])
	}
}

func TestDashboardAPI(t *testing.T) {
	srv, _ := newTestDashboard(t)
	code, body := get(t, srv.routes(), "/api/incidents")
	var recs []AuditRecord
	if code != http.StatusOK || json.Unmarshal([]byte(body), &recs) != nil || len(recs) != 3 {
		t.Fatalf("status %d, %d records", code, len(recs))
	}
}
