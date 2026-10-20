package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// deps are the backends the tools read from. Interfaces where tests need
// fakes; the Kubernetes clients take the client-go fakes directly.
type deps struct {
	kube interface {
		health(ctx context.Context, svc string) (*ServiceHealth, error)
		rollouts(ctx context.Context, svc string, limit int) ([]Revision, error)
		logs(ctx context.Context, svc string, since time.Duration, perPod int64) ([]LogLine, error)
	}
	prom interface {
		summary(ctx context.Context, name, svc, window string) (*MetricSummary, error)
		alerts(ctx context.Context, svc string) ([]FiringAlert, error)
	}
	git         gitHost
	environment string
	logger      *slog.Logger
}

func newMCPServer(d *deps, p *policy, m *metrics, logger *slog.Logger) *mcp.Server {
	if d.environment == "" {
		d.environment = envOr("ENVIRONMENT", "dev")
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "agentic-sre-mcp", Title: "Agentic SRE platform gateway", Version: version}, &mcp.ServerOptions{
		Instructions: serverInstructions,
		Logger:       logger,
	})
	s.AddReceivingMiddleware(governed(p, m, logger))
	d.addTools(s)
	d.addResources(s)
	addPrompts(s)
	return s
}

const serverInstructions = `Governed gateway to the agentic SRE platform. Start an investigation with
get_incident_context, read the service runbook resource, and drill down with
get_error_patterns and get_recent_logs. You cannot change the cluster: fixes
are proposed with create_pull_request and need CI and human approval. Every
call is authorised by policy (platform://policy/mcp) and audited.`

var readOnly = &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}

// --- inputs -------------------------------------------------------------

type ServiceInput struct {
	Service string `json:"service" jsonschema:"service name, e.g. payment-api"`
}

type WindowInput struct {
	Service string `json:"service" jsonschema:"service name, e.g. payment-api"`
	Window  string `json:"window,omitempty" jsonschema:"look-back window such as 30m or 2h (5m to 6h, default 30m)"`
}

type LogsInput struct {
	Service   string `json:"service" jsonschema:"service name, e.g. payment-api"`
	Window    string `json:"window,omitempty" jsonschema:"look-back window (default 15m)"`
	RequestID string `json:"request_id,omitempty" jsonschema:"only lines containing this request ID"`
	Contains  string `json:"contains,omitempty" jsonschema:"only lines containing this text"`
	Limit     int    `json:"limit,omitempty" jsonschema:"maximum lines returned (default 50, max 200)"`
}

type LimitInput struct {
	Service string `json:"service" jsonschema:"service name, e.g. payment-api"`
	Limit   int    `json:"limit,omitempty" jsonschema:"maximum entries (default 5, max 20)"`
}

type PullRequestNumber struct {
	Number           int    `json:"number" jsonschema:"pull request number returned by create_pull_request"`
	DeployedRevision string `json:"deployed_revision,omitempty" jsonschema:"optional Git commit SHA, e.g. Argo CD's synced revision; the result then says whether it includes the merge"`
}

type PatchInput struct {
	Service string `json:"service" jsonschema:"service name"`
	Patch   string `json:"patch" jsonschema:"strategic merge patch for the Deployment"`
}

// --- outputs ------------------------------------------------------------

type MetricsReport struct {
	Service string                    `json:"service"`
	Window  string                    `json:"window"`
	Metrics map[string]*MetricSummary `json:"metrics,omitempty"`
	Missing []string                  `json:"unavailable,omitempty"`
}

type LogsReport struct {
	Lines     []LogLine `json:"lines,omitempty"`
	Matched   int       `json:"matched"`
	Truncated bool      `json:"truncated,omitempty"`
}

type DeploymentsReport struct {
	Rollouts []Revision  `json:"rollouts,omitempty"`
	ArgoCD   *ArgoStatus `json:"argocd,omitempty"`
}

type GitReport struct {
	Paths   []string `json:"paths,omitempty"`
	Commits []Commit `json:"commits,omitempty"`
}

type IncidentContext struct {
	Service     string         `json:"service"`
	Alerts      []FiringAlert  `json:"alerts,omitempty"`
	Health      *ServiceHealth `json:"health,omitempty"`
	Metrics     *MetricsReport `json:"metrics,omitempty"`
	Errors      *PatternReport `json:"top_error_patterns,omitempty"`
	Deployments []Revision     `json:"recent_rollouts,omitempty"`
	Commits     []Commit       `json:"recent_commits,omitempty"`
	Resources   []string       `json:"read_next"`
	Unavailable []string       `json:"unavailable,omitempty"`
}

// --- registration -------------------------------------------------------

func (d *deps) addTools(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_incident_context",
		Description: "Start here. One summary of a service: alerts, health, error ratio and traffic, top error patterns, recent rollouts and commits, and which resources to read next.",
		Annotations: readOnly,
	}, d.incidentContext)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_service_health",
		Description: "Deployment replicas and conditions, every pod's readiness and restarts, recent warning events, and Argo CD sync/health.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ServiceInput) (*mcp.CallToolResult, *ServiceHealth, error) {
		h, err := d.kube.health(ctx, in.Service)
		return nil, h, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_metrics",
		Description: "Request rate, 5xx error ratio, p95 latency, ready pods and restarts: current value, min/max over the window, and the value at the window's start. Named queries only.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in WindowInput) (*mcp.CallToolResult, *MetricsReport, error) {
		return nil, d.metrics(ctx, in.Service, defaultString(in.Window, "30m")), nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_error_patterns",
		Description: "Error log lines grouped by normalised template, with counts, first/last seen, pods and sample request IDs. Use this before raw logs.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in WindowInput) (*mcp.CallToolResult, *PatternReport, error) {
		window := defaultString(in.Window, "30m")
		if err := validWindow(window); err != nil {
			return nil, nil, err
		}
		since, _ := time.ParseDuration(window)
		lines, err := d.kube.logs(ctx, in.Service, since, 2000)
		if err != nil {
			return nil, nil, err
		}
		rep := errorPatterns(lines, 10)
		return nil, &rep, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_recent_logs",
		Description: "Raw, redacted log lines, filtered by request ID or text. Capped; use get_error_patterns first and drill down with a request ID.",
		Annotations: readOnly,
	}, d.recentLogs)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_recent_deployments",
		Description: "Rollout history from ReplicaSets, newest first: images, env vars (secret values never shown) and what changed from the previous revision, plus Argo CD's synced Git revision and history.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in LimitInput) (*mcp.CallToolResult, *DeploymentsReport, error) {
		revs, err := d.kube.rollouts(ctx, in.Service, clamp(in.Limit, 5, 20))
		if err != nil {
			return nil, nil, err
		}
		h, _ := d.kube.health(ctx, in.Service)
		rep := &DeploymentsReport{Rollouts: revs}
		if h != nil {
			rep.ArgoCD = h.ArgoCD
		}
		return nil, rep, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_git_changes",
		Description: "Recent commits on the main branch touching the service's manifests or code, with per-file diffs (truncated).",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in LimitInput) (*mcp.CallToolResult, *GitReport, error) {
		paths := d.servicePaths(in.Service)
		commits, err := d.git.recentCommits(ctx, paths, clamp(in.Limit, 5, 20))
		if err != nil {
			return nil, nil, err
		}
		return nil, &GitReport{Paths: paths, Commits: commits}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "create_pull_request",
		Description: "Propose a change. The only write available: files must be under gitops/workloads/ for an in-scope service, " +
			"the branch must start with agent/, and Secrets are refused. The PR needs CI and human approval before it can merge.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in PullRequestInput) (*mcp.CallToolResult, *PullRequestResult, error) {
		if strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.Body) == "" {
			return nil, nil, errors.New("title and body are required; the body must carry the hypothesis and evidence")
		}
		pr, err := d.git.createPullRequest(ctx, in)
		return nil, pr, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_pull_request",
		Description: "Status of a pull request opened from an agent/* branch: open or closed, whether it was merged, by whom and when, and its merge commit. With deployed_revision, also whether that revision includes the merge.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in PullRequestNumber) (*mcp.CallToolResult, *PullRequestStatus, error) {
		st, err := d.git.pullRequest(ctx, in.Number)
		if err != nil || in.DeployedRevision == "" || !st.Merged || st.MergeSHA == "" {
			return nil, st, err
		}
		if !commitSHA.MatchString(in.DeployedRevision) {
			return nil, nil, errors.New("deployed_revision must be a hex commit SHA")
		}
		deployed := strings.HasPrefix(st.MergeSHA, in.DeployedRevision)
		if !deployed {
			if deployed, err = d.git.containsCommit(ctx, in.DeployedRevision, st.MergeSHA); err != nil {
				return nil, nil, err
			}
		}
		st.MergeDeployed = &deployed
		return nil, st, nil
	})

	// Exists to be refused. The policy denies it before this handler runs;
	// the handler refuses as well, in case the policy is ever misconfigured.
	mcp.AddTool(s, &mcp.Tool{
		Name:        "patch_deployment",
		Description: "Directly patch a running Deployment. Prohibited by platform policy; changes must go through create_pull_request.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(true)},
	}, func(context.Context, *mcp.CallToolRequest, PatchInput) (*mcp.CallToolResult, *decision, error) {
		return nil, &decision{Reason: "Direct production mutation is prohibited.", RequiredPath: "create_pull_request"}, nil
	})
}

func (d *deps) servicePaths(svc string) []string {
	return []string{fmt.Sprintf("gitops/workloads/%s/%s", d.environment, svc), "apps/" + svc}
}

func (d *deps) metrics(ctx context.Context, svc, window string) *MetricsReport {
	rep := &MetricsReport{Service: svc, Window: window, Metrics: map[string]*MetricSummary{}}
	for _, name := range []string{"error_ratio", "request_rate", "p95_latency_seconds", "ready_pods", "container_restarts"} {
		s, err := d.prom.summary(ctx, name, svc, window)
		if err != nil {
			rep.Missing = append(rep.Missing, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		rep.Metrics[name] = s
	}
	return rep
}

func (d *deps) recentLogs(ctx context.Context, _ *mcp.CallToolRequest, in LogsInput) (*mcp.CallToolResult, *LogsReport, error) {
	window := defaultString(in.Window, "15m")
	if err := validWindow(window); err != nil {
		return nil, nil, err
	}
	since, _ := time.ParseDuration(window)
	lines, err := d.kube.logs(ctx, in.Service, since, 1000)
	if err != nil {
		return nil, nil, err
	}
	limit := clamp(in.Limit, 50, 200)
	rep := &LogsReport{}
	for _, l := range lines {
		if in.RequestID != "" && !strings.Contains(l.Text, in.RequestID) {
			continue
		}
		if in.Contains != "" && !strings.Contains(l.Text, in.Contains) {
			continue
		}
		rep.Matched++
		rep.Lines = append(rep.Lines, l)
	}
	if len(rep.Lines) > limit { // keep the most recent
		rep.Lines = rep.Lines[len(rep.Lines)-limit:]
		rep.Truncated = true
	}
	return nil, rep, nil
}

// incidentContext fans out to every backend and degrades gracefully: one
// unavailable source is reported, not fatal.
func (d *deps) incidentContext(ctx context.Context, _ *mcp.CallToolRequest, in ServiceInput) (*mcp.CallToolResult, *IncidentContext, error) {
	ic := &IncidentContext{
		Service: in.Service,
		Resources: []string{
			"platform://services/" + in.Service + "/runbook",
			"platform://services/" + in.Service + "/readme",
		},
	}
	miss := func(what string, err error) {
		ic.Unavailable = append(ic.Unavailable, fmt.Sprintf("%s: %v", what, err))
	}

	if a, err := d.prom.alerts(ctx, in.Service); err != nil {
		miss("alerts", err)
	} else {
		ic.Alerts = a
	}
	if h, err := d.kube.health(ctx, in.Service); err != nil {
		miss("health", err)
	} else {
		ic.Health = h
	}
	ic.Metrics = d.metrics(ctx, in.Service, "30m")
	if lines, err := d.kube.logs(ctx, in.Service, 30*time.Minute, 2000); err != nil {
		miss("error patterns", err)
	} else {
		rep := errorPatterns(lines, 3)
		ic.Errors = &rep
	}
	if revs, err := d.kube.rollouts(ctx, in.Service, 3); err != nil {
		miss("rollouts", err)
	} else {
		ic.Deployments = revs
	}
	if commits, err := d.git.recentCommits(ctx, d.servicePaths(in.Service), 3); err != nil {
		miss("git", err)
	} else {
		ic.Commits = commits
	}
	return nil, ic, nil
}

func defaultString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func clamp(v, fallback, max int) int {
	if v <= 0 {
		return fallback
	}
	if v > max {
		return max
	}
	return v
}

func boolPtr(b bool) *bool { return &b }

var commitSHA = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
