package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// The orchestrator is deliberately weak: it receives webhooks, de-duplicates,
// and creates Jobs from a fixed template. It has no model access and no AWS
// role; RBAC lets it manage Jobs (and read their Pods) in its own namespace
// only, and a ValidatingAdmissionPolicy pins what those Jobs may contain.

const (
	labelComponent   = "app.kubernetes.io/name"
	workerComponent  = "sre-agent-worker"
	labelIncidentKey = "sre.calebache.com/incident-key"
	annService       = "sre.calebache.com/service"
	annAlert         = "sre.calebache.com/alert"
)

type orchestratorConfig struct {
	namespace, workerSA, image string
	allowedServices            map[string]bool
	cooldown                   time.Duration
	activeDeadline             int64
	workerEnv                  []corev1.EnvVar
}

type orchestrator struct {
	cfg     orchestratorConfig
	kube    kubernetes.Interface
	logger  *slog.Logger
	now     func() time.Time
	metrics *orchMetrics
}

type orchMetrics struct {
	registry  *prometheus.Registry
	alerts    *prometheus.CounterVec
	decisions *prometheus.CounterVec
	incidents *prometheus.GaugeVec
	active    prometheus.Gauge
}

func newOrchMetrics() *orchMetrics {
	m := &orchMetrics{
		registry: prometheus.NewRegistry(),
		alerts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sre_agent_alerts_received_total", Help: "Alerts received from Alertmanager, by status.",
		}, []string{"status"}),
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sre_agent_alert_decisions_total", Help: "What the orchestrator did with each firing alert.",
		}, []string{"decision"}),
		incidents: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sre_agent_incidents", Help: "Incident Jobs that finished in the last 24h, by service and outcome.",
		}, []string{"service", "outcome"}),
		active: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sre_agent_active_investigations", Help: "Incident Jobs currently running.",
		}),
	}
	m.registry.MustRegister(m.alerts, m.decisions, m.incidents, m.active)
	return m
}

func runOrchestrator(ctx context.Context, logger *slog.Logger) error {
	image, err := mustEnv("AGENT_IMAGE")
	if err != nil {
		return err
	}
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return err
	}
	cfg := orchestratorConfig{
		namespace:       envOr("POD_NAMESPACE", "sre-agent"),
		workerSA:        envOr("WORKER_SERVICE_ACCOUNT", "sre-agent"),
		image:           image,
		allowedServices: map[string]bool{},
		cooldown:        durationEnv("INCIDENT_COOLDOWN", 30*time.Minute),
		activeDeadline:  int64(intEnv("INCIDENT_DEADLINE_SECONDS", 3600)),
	}
	for _, s := range strings.Split(envOr("ALLOWED_SERVICES", "payment-api"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			cfg.allowedServices[s] = true
		}
	}
	// Worker configuration is passed through verbatim; the orchestrator itself
	// uses none of it.
	for _, k := range []string{"BEDROCK_REGION", "MODEL_ID", "AUDIT_BUCKET", "AWS_REGION", "ENVIRONMENT", "MCP_ENDPOINT",
		"DEMO_ATTEMPT_DIRECT_FIX", "MERGE_TIMEOUT", "VERIFY_INTERVAL", "VERIFY_ATTEMPTS", "MAX_TOOL_CALLS", "MAX_MODEL_CALLS", "MAX_OUTPUT_TOKENS"} {
		if v := envOr("WORKER_"+k, ""); v != "" {
			cfg.workerEnv = append(cfg.workerEnv, corev1.EnvVar{Name: k, Value: v})
		}
	}

	o := &orchestrator{cfg: cfg, kube: kube, logger: logger, now: time.Now, metrics: newOrchMetrics()}
	go o.reconcileLoop(ctx, 30*time.Second)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /alerts", o.handleAlerts)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	api := &http.Server{Addr: envOr("LISTEN_ADDR", ":8080"), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	metricsSrv := &http.Server{Addr: envOr("METRICS_ADDR", ":9090"), Handler: promhttp.HandlerFor(o.metrics.registry, promhttp.HandlerOpts{}), ReadHeaderTimeout: 5 * time.Second}
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
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return errors.Join(api.Shutdown(sctx), metricsSrv.Shutdown(sctx))
}

// webhook is the part of Alertmanager's webhook payload (version 4) we use.
type webhook struct {
	Alerts []struct {
		Status string            `json:"status"`
		Labels map[string]string `json:"labels"`
	} `json:"alerts"`
}

func (o *orchestrator) handleAlerts(w http.ResponseWriter, r *http.Request) {
	var wh webhook
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&wh); err != nil {
		http.Error(w, "invalid webhook payload", http.StatusBadRequest)
		return
	}
	failed := false
	for _, a := range wh.Alerts {
		o.metrics.alerts.WithLabelValues(a.Status).Inc()
		if a.Status != "firing" {
			continue // workers verify recovery themselves
		}
		decision, err := o.consider(r.Context(), a.Labels["service"], a.Labels["alertname"], a.Labels["severity"])
		o.metrics.decisions.WithLabelValues(decision).Inc()
		if err != nil {
			failed = true
			o.logger.Error("could not start investigation", "service", a.Labels["service"], "alert", a.Labels["alertname"], "error", err)
		}
	}
	if failed { // Alertmanager retries on 5xx
		http.Error(w, "failed to start one or more investigations", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

var labelSafe = regexp.MustCompile(`[^a-z0-9-]`)

// consider decides whether a firing alert starts a new incident Job.
func (o *orchestrator) consider(ctx context.Context, service, alert, severity string) (string, error) {
	switch {
	case !o.cfg.allowedServices[service]:
		return "ignored_out_of_scope", nil
	case alert == "":
		return "ignored_no_alertname", nil
	case severity != "critical":
		return "ignored_severity", nil
	}
	key := incidentKey(service, alert)
	jobs, err := o.kube.BatchV1().Jobs(o.cfg.namespace).List(ctx, metav1.ListOptions{LabelSelector: labelIncidentKey + "=" + key})
	if err != nil {
		return "error", fmt.Errorf("list jobs: %w", err)
	}
	for _, j := range jobs.Items {
		if !jobFinished(&j) {
			return "deduplicated_active", nil
		}
		if o.now().Sub(j.CreationTimestamp.Time) < o.cfg.cooldown {
			return "deduplicated_cooldown", nil
		}
	}
	job := o.buildJob(service, alert, key)
	if _, err := o.kube.BatchV1().Jobs(o.cfg.namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return "error", fmt.Errorf("create job: %w", err)
	}
	o.logger.Info("investigation started", "job", job.Name, "service", service, "alert", alert)
	return "started", nil
}

func incidentKey(service, alert string) string {
	h := sha256.Sum256([]byte(service + "/" + alert))
	return hex.EncodeToString(h[:])[:16]
}

// buildJob is the only Job shape the orchestrator can produce. The admission
// policy in the cluster enforces the same shape independently.
func (o *orchestrator) buildJob(service, alert, key string) *batchv1.Job {
	rnd := make([]byte, 3)
	_, _ = rand.Read(rnd)
	id := fmt.Sprintf("inc-%s-%s", o.now().UTC().Format("20060102-150405"), hex.EncodeToString(rnd))
	name := truncate(labelSafe.ReplaceAllString("sre-"+strings.ToLower(service), "-"), 30)
	name = strings.TrimSuffix(name, "…(truncated)") + "-" + hex.EncodeToString(rnd)

	f, t := false, true
	zero, uid := int32(0), int64(65532)
	ttl := int32(24 * 3600)
	expiry := int64(3600)
	labels := map[string]string{labelComponent: workerComponent, labelIncidentKey: key, "app.kubernetes.io/part-of": "agentic-sre"}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: o.cfg.namespace, Labels: labels,
			Annotations: map[string]string{annService: service, annAlert: alert, "sre.calebache.com/incident-id": id},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &zero, // an incident is never silently retried
			ActiveDeadlineSeconds:   &o.cfg.activeDeadline,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName:           o.cfg.workerSA,
					AutomountServiceAccountToken: &f,
					RestartPolicy:                corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &t, RunAsUser: &uid, RunAsGroup: &uid,
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:  "investigate",
						Image: o.cfg.image,
						Args:  []string{"investigate", "--service", service, "--alert", alert, "--incident-id", id},
						Env:   o.cfg.workerEnv,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &f, ReadOnlyRootFilesystem: &t,
							Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						TerminationMessagePolicy: corev1.TerminationMessageReadFile,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: "mcp-token", MountPath: "/var/run/secrets/mcp", ReadOnly: true}},
					}},
					// The only credential mounted: a short-lived token for the
					// MCP server's audience. AWS credentials come from Pod Identity.
					Volumes: []corev1.Volume{{
						Name: "mcp-token",
						VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
							Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
								Audience: "mcp-server", ExpirationSeconds: &expiry, Path: "token",
							}}},
						}},
					}},
				},
			},
		},
	}
}

func jobFinished(j *batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// reconcileLoop turns finished Jobs into outcome metrics, read from each
// worker pod's termination message.
func (o *orchestrator) reconcileLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		o.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (o *orchestrator) reconcile(ctx context.Context) {
	jobs, err := o.kube.BatchV1().Jobs(o.cfg.namespace).List(ctx, metav1.ListOptions{LabelSelector: labelComponent + "=" + workerComponent})
	if err != nil {
		o.logger.Warn("reconcile: list jobs", "error", err)
		return
	}
	counts := map[[2]string]float64{}
	active := 0
	for _, j := range jobs.Items {
		if !jobFinished(&j) {
			active++
			continue
		}
		if o.now().Sub(j.CreationTimestamp.Time) > 24*time.Hour {
			continue
		}
		counts[[2]string{j.Annotations[annService], o.outcome(ctx, &j)}]++
	}
	o.metrics.incidents.Reset()
	keys := make([][2]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i][0]+keys[i][1] < keys[j][0]+keys[j][1] })
	for _, k := range keys {
		o.metrics.incidents.WithLabelValues(k[0], k[1]).Set(counts[k])
	}
	o.metrics.active.Set(float64(active))
}

func (o *orchestrator) outcome(ctx context.Context, j *batchv1.Job) string {
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue && c.Reason == "DeadlineExceeded" {
			return "deadline_exceeded"
		}
	}
	pods, err := o.kube.CoreV1().Pods(o.cfg.namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + j.Name})
	if err != nil {
		return "unknown"
	}
	for _, p := range pods.Items {
		for _, cs := range p.Status.ContainerStatuses {
			if term := cs.State.Terminated; term != nil && term.Message != "" {
				var m struct {
					Outcome string `json:"outcome"`
				}
				if json.Unmarshal([]byte(term.Message), &m) == nil && m.Outcome != "" {
					return m.Outcome
				}
			}
		}
	}
	return "unknown"
}
