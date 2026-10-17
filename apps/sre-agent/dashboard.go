package main

// The dashboard mode: a small read-only web page over the audit trail. It
// shows what the agent did in each incident (tool calls, policy denials,
// the PR) and what it recommends, straight from the S3 audit records the
// workers write. It has no model, MCP or Kubernetes access; its AWS role can
// only list and read audit/*.

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

//go:embed dashboard/*.html
var dashboardTemplates embed.FS

// auditReader is the read side of the audit bucket.
type auditReader interface {
	list(ctx context.Context) ([]auditObject, error)
	get(ctx context.Context, key string) ([]byte, error)
}

type auditObject struct {
	Key, ETag string
	Modified  time.Time
}

type s3Reader struct {
	client *s3.Client
	bucket string
}

func (r *s3Reader) list(ctx context.Context) ([]auditObject, error) {
	var out []auditObject
	p := s3.NewListObjectsV2Paginator(r.client, &s3.ListObjectsV2Input{Bucket: aws.String(r.bucket), Prefix: aws.String("audit/")})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			// audit/whoami/ holds whoami's write probes, not incidents.
			if k := aws.ToString(o.Key); strings.HasSuffix(k, ".json") && !strings.HasPrefix(k, "audit/whoami/") {
				out = append(out, auditObject{Key: aws.ToString(o.Key), ETag: aws.ToString(o.ETag), Modified: aws.ToTime(o.LastModified)})
			}
		}
	}
	return out, nil
}

func (r *s3Reader) get(ctx context.Context, key string) ([]byte, error) {
	o, err := r.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	defer o.Body.Close()
	return io.ReadAll(io.LimitReader(o.Body, 4<<20))
}

// incidentStore keeps the parsed records, refetching only objects whose
// ETag changed, and lists the bucket at most once per refresh interval.
type incidentStore struct {
	reader   auditReader
	refresh  time.Duration
	limit    int
	logger   *slog.Logger
	now      func() time.Time
	mu       sync.Mutex
	loadedAt time.Time
	byKey    map[string]cachedRecord
	sorted   []AuditRecord
	lastErr  error
}

type cachedRecord struct {
	etag string
	rec  AuditRecord
}

func newIncidentStore(r auditReader, refresh time.Duration, limit int, logger *slog.Logger) *incidentStore {
	return &incidentStore{reader: r, refresh: refresh, limit: limit, logger: logger, now: time.Now, byKey: map[string]cachedRecord{}}
}

// incidents returns records newest first, and the last load error (stale
// data is still served if a refresh fails).
func (s *incidentStore) incidents(ctx context.Context) ([]AuditRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadedAt.IsZero() || s.now().Sub(s.loadedAt) >= s.refresh {
		s.lastErr = s.load(ctx)
		s.loadedAt = s.now()
	}
	return s.sorted, s.lastErr
}

func (s *incidentStore) load(ctx context.Context) error {
	objs, err := s.reader.list(ctx)
	if err != nil {
		return fmt.Errorf("list audit records: %w", err)
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].Modified.After(objs[j].Modified) })
	if s.limit > 0 && len(objs) > s.limit {
		objs = objs[:s.limit]
	}
	next := make(map[string]cachedRecord, len(objs))
	var errs []error
	for _, o := range objs {
		if c, ok := s.byKey[o.Key]; ok && c.etag == o.ETag {
			next[o.Key] = c
			continue
		}
		body, err := s.reader.get(ctx, o.Key)
		if err == nil {
			var rec AuditRecord
			if err = json.Unmarshal(body, &rec); err == nil {
				if rec.ExecutionID != "" { // anything else isn't an incident record
					next[o.Key] = cachedRecord{etag: o.ETag, rec: rec}
				}
				continue
			}
		}
		errs = append(errs, fmt.Errorf("%s: %w", o.Key, err))
		if c, ok := s.byKey[o.Key]; ok { // keep the last good version
			next[o.Key] = c
		}
	}
	s.byKey = next
	s.sorted = s.sorted[:0]
	for _, c := range next {
		s.sorted = append(s.sorted, c.rec)
	}
	sort.Slice(s.sorted, func(i, j int) bool { return s.sorted[i].StartedAt > s.sorted[j].StartedAt })
	if len(errs) > 0 {
		s.logger.Warn("some audit records could not be read", "count", len(errs), "first", errs[0])
		return errors.Join(errs...)
	}
	return nil
}

// --- view model -------------------------------------------------------------

type incidentView struct {
	AuditRecord
	Started, Ago, Duration string
	Running                bool
	Tone                   string // ok | warn | bad | info | muted
	Next                   string // the recommendation, or the best substitute
	Phases                 []phaseView
	Denials, Reads         int
}

type phaseView struct {
	Name  string
	Calls []ToolCall
}

type summaryView struct {
	Total, Resolved, NeedsHuman, Running, PRs, Denials, ModelCalls int
	Tokens                                                         int64
}

func outcomeTone(outcome string, running bool) string {
	switch {
	case running:
		return "info"
	case outcome == "resolved":
		return "ok"
	case outcome == "no_action":
		return "muted"
	case outcome == "failed" || outcome == "not_recovered" || outcome == "stopped":
		return "bad"
	default: // escalated, pr_rejected, approval_timeout
		return "warn"
	}
}

// A worker Job is killed after an hour (activeDeadlineSeconds); a record
// still unfinished well past that will never finish, e.g. the environment
// was torn down mid-incident.
const workerGiveUp = 75 * time.Minute

func viewOf(r AuditRecord, now time.Time) incidentView {
	v := incidentView{AuditRecord: r}
	started, startErr := time.Parse(time.RFC3339, r.StartedAt)
	if r.FinishedAt == "" && r.Outcome == "" {
		if startErr == nil && now.Sub(started) > workerGiveUp {
			v.Outcome = "stopped"
			v.Next = "The worker stopped without a result (its Job hit the one-hour limit, or the environment went away). Check the Job's logs if it still exists, and the service's current health."
		} else {
			v.Running = true
			v.Outcome = "in progress"
		}
	}
	v.Tone = outcomeTone(v.Outcome, v.Running)
	if t, err := started, startErr; err == nil {
		v.Started = t.UTC().Format("2006-01-02 15:04 UTC")
		v.Ago = ago(now.Sub(t))
		end := now
		if f, err := time.Parse(time.RFC3339, r.FinishedAt); err == nil {
			end = f
		}
		v.Duration = end.Sub(t).Round(time.Second).String()
	}
	if r.Recommendation != "" {
		v.Next = r.Recommendation
	}
	if v.Next == "" { // records from before the recommendation field
		for _, line := range r.Verification {
			if s, ok := strings.CutPrefix(line, "remediation: "); ok {
				v.Next = s
			}
		}
	}
	if v.Next == "" && r.ProposedAction != "" {
		v.Next = "Review and merge the proposed pull request."
	}
	for _, c := range r.ToolCalls {
		if c.Decision == "denied" {
			v.Denials++
		}
		if c.Tool == readResourceTool {
			v.Reads++
		}
		if n := len(v.Phases); n == 0 || v.Phases[n-1].Name != c.Phase {
			v.Phases = append(v.Phases, phaseView{Name: c.Phase})
		}
		v.Phases[len(v.Phases)-1].Calls = append(v.Phases[len(v.Phases)-1].Calls, c)
	}
	return v
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func summarize(vs []incidentView) summaryView {
	var s summaryView
	for _, v := range vs {
		s.Total++
		switch v.Tone {
		case "ok":
			s.Resolved++
		case "info":
			s.Running++
		case "warn", "bad":
			s.NeedsHuman++
		}
		if v.ProposedAction != "" {
			s.PRs++
		}
		s.Denials += v.Denials
		s.ModelCalls += v.Usage.ModelCalls
		s.Tokens += v.Usage.InputTokens + v.Usage.OutputTokens
	}
	return s
}

// inlineMarkdown renders the little Markdown the agent writes (`code` and
// **bold**) after escaping everything, so agent-written text, which comes
// from logs and commits, can never inject markup.
var (
	mdCode = regexp.MustCompile("`([^`\n]+)`")
	mdBold = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
)

func inlineMarkdown(s string) template.HTML {
	out := html.EscapeString(s)
	out = mdCode.ReplaceAllString(out, "<code>$1</code>")
	out = mdBold.ReplaceAllString(out, "<strong>$1</strong>")
	return template.HTML(out) //nolint:gosec // escaped above; only <code> and <strong> are added
}

// --- HTTP -------------------------------------------------------------------

type dashboardServer struct {
	store *incidentStore
	pages *template.Template
	env   string
	now   func() time.Time
}

func newDashboardServer(store *incidentStore, env string) (*dashboardServer, error) {
	pages, err := template.New("").Funcs(template.FuncMap{
		"md": inlineMarkdown,
		"approval": func(b *bool) string {
			switch {
			case b == nil:
				return "—"
			case *b:
				return "Yes"
			}
			return "No"
		},
		"short": func(s string, n int) string {
			if r := []rune(s); len(r) > n {
				return string(r[:n]) + "…"
			}
			return s
		},
	}).ParseFS(dashboardTemplates, "dashboard/*.html")
	if err != nil {
		return nil, err
	}
	return &dashboardServer{store: store, pages: pages, env: env, now: time.Now}, nil
}

func (d *dashboardServer) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", d.index)
	mux.HandleFunc("GET /incidents/{id}", d.incident)
	mux.HandleFunc("GET /api/incidents", d.api)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	return mux
}

func (d *dashboardServer) views(r *http.Request) ([]incidentView, error) {
	recs, err := d.store.incidents(r.Context())
	now := d.now()
	out := make([]incidentView, 0, len(recs))
	for _, rec := range recs {
		out = append(out, viewOf(rec, now))
	}
	return out, err
}

func (d *dashboardServer) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// No scripts, no external resources: the page is static HTML and CSS.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := d.pages.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("render", "page", name, "error", err)
	}
}

func (d *dashboardServer) index(w http.ResponseWriter, r *http.Request) {
	vs, err := d.views(r)
	d.render(w, "index.html", map[string]any{
		"Env": d.env, "Incidents": vs, "Summary": summarize(vs), "Err": loadWarning(err),
		"Now": d.now().UTC().Format("15:04:05 UTC"),
	})
}

func (d *dashboardServer) incident(w http.ResponseWriter, r *http.Request) {
	vs, err := d.views(r)
	id := r.PathValue("id")
	for _, v := range vs {
		if v.ExecutionID == id {
			d.render(w, "incident.html", map[string]any{"Env": d.env, "I": v, "Err": loadWarning(err)})
			return
		}
	}
	http.NotFound(w, r)
}

func (d *dashboardServer) api(w http.ResponseWriter, r *http.Request) {
	recs, err := d.store.incidents(r.Context())
	if err != nil && len(recs) == 0 {
		http.Error(w, "audit records unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(recs)
}

func loadWarning(err error) string {
	if err == nil {
		return ""
	}
	return "Some audit records could not be loaded; showing what was read. See the dashboard's logs."
}

func runDashboard(ctx context.Context, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("dashboard", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "listen address")
	refresh := fs.Duration("refresh", 15*time.Second, "how often to re-list the audit bucket")
	limit := fs.Int("limit", 200, "most recent audit records to show")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bucket, err := mustEnv("AUDIT_BUCKET")
	if err != nil {
		return err
	}
	// Region comes from AWS_REGION (set on the Deployment); pods can't read
	// it from instance metadata.
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}
	store := newIncidentStore(&s3Reader{client: s3.NewFromConfig(cfg), bucket: bucket}, *refresh, *limit, logger)
	srv, err := newDashboardServer(store, envOr("ENVIRONMENT", "dev"))
	if err != nil {
		return err
	}
	httpSrv := &http.Server{Addr: *addr, Handler: srv.routes(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
	}()
	logger.Info("dashboard listening", "addr", *addr, "bucket", bucket)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
