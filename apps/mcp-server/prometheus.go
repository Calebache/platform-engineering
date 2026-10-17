package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

// The agent can't send PromQL. It picks from named queries, so it can't run
// expensive or cluster-wide queries, and every number it sees has a known
// meaning.
const probeRoutes = `route!~"GET /(healthz|readyz)"`

var namedQueries = map[string]string{
	"request_rate":        `sum(rate(http_requests_total{namespace=%[1]q, service=%[1]q, ` + probeRoutes + `}[5m]))`,
	"error_ratio":         `(sum(rate(http_requests_total{namespace=%[1]q, service=%[1]q, code=~"5..", ` + probeRoutes + `}[5m])) or vector(0)) / sum(rate(http_requests_total{namespace=%[1]q, service=%[1]q, ` + probeRoutes + `}[5m]))`,
	"p95_latency_seconds": `histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{namespace=%[1]q, service=%[1]q, ` + probeRoutes + `}[5m])))`,
	"ready_pods":          `sum(kube_pod_status_ready{namespace=%[1]q, condition="true"})`,
	"container_restarts":  `sum(increase(kube_pod_container_status_restarts_total{namespace=%[1]q}[15m]))`,
}

var windowRE = regexp.MustCompile(`^([1-9][0-9]?)(m|h)$`)

type promBackend struct {
	baseURL string
	http    *http.Client
}

type MetricSummary struct {
	Query         string   `json:"query"`
	Current       *float64 `json:"current"`
	WindowMax     *float64 `json:"max_over_window"`
	WindowMin     *float64 `json:"min_over_window"`
	AtWindowStart *float64 `json:"at_window_start"`
}

// summary evaluates a named query now, at the start of the window, and its
// min/max across the window (1-minute resolution).
func (p *promBackend) summary(ctx context.Context, name, svc, window string) (*MetricSummary, error) {
	tmpl, ok := namedQueries[name]
	if !ok {
		return nil, fmt.Errorf("unknown query %q", name)
	}
	if err := validWindow(window); err != nil {
		return nil, err
	}
	expr := fmt.Sprintf(tmpl, svc)
	s := &MetricSummary{Query: name}
	var err error
	if s.Current, err = p.scalar(ctx, expr); err != nil {
		return nil, err
	}
	s.WindowMax, _ = p.scalar(ctx, fmt.Sprintf("max_over_time((%s)[%s:1m])", expr, window))
	s.WindowMin, _ = p.scalar(ctx, fmt.Sprintf("min_over_time((%s)[%s:1m])", expr, window))
	s.AtWindowStart, _ = p.scalar(ctx, fmt.Sprintf("(%s) offset %s", expr, window))
	return s, nil
}

func validWindow(w string) error {
	m := windowRE.FindStringSubmatch(w)
	if m == nil {
		return fmt.Errorf("window %q must look like 15m or 2h", w)
	}
	n, _ := strconv.Atoi(m[1])
	if (m[2] == "h" && n > 6) || (m[2] == "m" && n < 5) {
		return fmt.Errorf("window %q must be between 5m and 6h", w)
	}
	return nil
}

// scalar runs an instant query and returns the first sample, or nil when the
// result is empty (e.g. no traffic).
func (p *promBackend) scalar(ctx context.Context, expr string) (*float64, error) {
	u := p.baseURL + "/api/v1/query?" + url.Values{"query": {expr}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus: %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Value [2]any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("prometheus: decode: %w", err)
	}
	if body.Status != "success" {
		return nil, fmt.Errorf("prometheus: %s", body.Error)
	}
	if len(body.Data.Result) == 0 {
		return nil, nil
	}
	str, _ := body.Data.Result[0].Value[1].(string)
	v, err := strconv.ParseFloat(str, 64)
	if err != nil {
		return nil, nil // NaN/Inf from an empty ratio
	}
	return &v, nil
}

type FiringAlert struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Severity string `json:"severity"`
	Since    string `json:"active_since"`
	Summary  string `json:"summary"`
}

// alerts returns pending and firing alerts labelled with this service.
func (p *promBackend) alerts(ctx context.Context, svc string) ([]FiringAlert, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/api/v1/alerts", nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus: %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		Data struct {
			Alerts []struct {
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
				State       string            `json:"state"`
				ActiveAt    time.Time         `json:"activeAt"`
			} `json:"alerts"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	var out []FiringAlert
	for _, a := range body.Data.Alerts {
		if a.Labels["service"] != svc {
			continue
		}
		out = append(out, FiringAlert{
			Name: a.Labels["alertname"], State: a.State, Severity: a.Labels["severity"],
			Since: a.ActiveAt.UTC().Format(time.RFC3339), Summary: a.Annotations["summary"],
		})
	}
	return out, nil
}
