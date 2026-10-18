package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newTestOrchestrator() (*orchestrator, *fake.Clientset, *time.Time) {
	now := time.Date(2026, 10, 17, 16, 0, 0, 0, time.UTC)
	kube := fake.NewClientset()
	o := &orchestrator{
		cfg: orchestratorConfig{
			namespace: "sre-agent", workerSA: "sre-agent", image: "ecr/sre-agent@sha256:abc",
			allowedServices: map[string]bool{"payment-api": true}, cooldown: 30 * time.Minute, activeDeadline: 3600,
		},
		kube: kube, logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), metrics: newOrchMetrics(),
		now: func() time.Time { return now },
	}
	return o, kube, &now
}

func jobs(t *testing.T, kube *fake.Clientset) []batchv1.Job {
	t.Helper()
	l, err := kube.BatchV1().Jobs("sre-agent").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return l.Items
}

func TestWebhookStartsOneJobPerIncident(t *testing.T) {
	o, kube, _ := newTestOrchestrator()
	payload := `{"alerts":[
		{"status":"firing","labels":{"alertname":"PaymentApiHighErrorRate","service":"payment-api","severity":"critical"}},
		{"status":"firing","labels":{"alertname":"PaymentApiHighErrorRate","service":"payment-api","severity":"critical"}},
		{"status":"firing","labels":{"alertname":"KubePodCrashLooping","service":"kube-system","severity":"critical"}},
		{"status":"firing","labels":{"alertname":"McpPolicyDenials","service":"payment-api","severity":"warning"}},
		{"status":"resolved","labels":{"alertname":"PaymentApiHighErrorRate","service":"payment-api","severity":"critical"}}
	]}`
	rec := httptest.NewRecorder()
	o.handleAlerts(rec, httptest.NewRequest(http.MethodPost, "/alerts", strings.NewReader(payload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	js := jobs(t, kube)
	if len(js) != 1 {
		t.Fatalf("%d jobs created, want 1 (duplicate, out-of-scope, non-critical and resolved alerts start nothing)", len(js))
	}

	j := js[0]
	pod := j.Spec.Template.Spec
	c := pod.Containers[0]
	switch {
	case pod.ServiceAccountName != "sre-agent", pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken:
		t.Error("worker identity wrong")
	case c.Image != "ecr/sre-agent@sha256:abc" || c.Args[0] != "investigate" || !strings.Contains(strings.Join(c.Args, " "), "--service payment-api --alert PaymentApiHighErrorRate"):
		t.Errorf("worker command wrong: %s %v", c.Image, c.Args)
	case *j.Spec.BackoffLimit != 0 || *j.Spec.ActiveDeadlineSeconds != 3600:
		t.Error("job limits wrong")
	case pod.Volumes[0].Projected.Sources[0].ServiceAccountToken.Audience != "mcp-server":
		t.Error("MCP token audience wrong")
	case c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem:
		t.Error("root filesystem must be read-only")
	}
}

func TestDedupeActiveAndCooldown(t *testing.T) {
	o, kube, now := newTestOrchestrator()
	ctx := context.Background()
	start := func() string {
		d, err := o.consider(ctx, "payment-api", "PaymentApiHighErrorRate", "critical")
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if d := start(); d != "started" {
		t.Fatalf("first: %s", d)
	}
	if d := start(); d != "deduplicated_active" {
		t.Fatalf("while running: %s", d)
	}

	// Mark it finished; inside the cooldown it's still deduplicated.
	j := jobs(t, kube)[0]
	j.CreationTimestamp = metav1.NewTime(*now)
	j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if _, err := kube.BatchV1().Jobs("sre-agent").Update(ctx, &j, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(10 * time.Minute)
	if d := start(); d != "deduplicated_cooldown" {
		t.Fatalf("in cooldown: %s", d)
	}
	*now = now.Add(time.Hour)
	if d := start(); d != "started" {
		t.Fatalf("after cooldown: %s", d)
	}
}

func TestReconcileReadsOutcomes(t *testing.T) {
	o, kube, now := newTestOrchestrator()
	ctx := context.Background()
	if _, err := o.consider(ctx, "payment-api", "PaymentApiHighErrorRate", "critical"); err != nil {
		t.Fatal(err)
	}
	j := jobs(t, kube)[0]
	j.CreationTimestamp = metav1.NewTime(*now)
	j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	_, _ = kube.BatchV1().Jobs("sre-agent").Update(ctx, &j, metav1.UpdateOptions{})
	_, _ = kube.CoreV1().Pods("sre-agent").Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: j.Name + "-x", Namespace: "sre-agent", Labels: map[string]string{"job-name": j.Name}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{Message: `{"outcome":"escalated","hypothesis":"DB auth failed\n<!channel> after rotation"}`}}}}},
	}, metav1.CreateOptions{})

	o.reconcile(ctx)
	if got := o.outcome(ctx, &j); got != "escalated" {
		t.Fatalf("outcome %q", got)
	}
	id := j.Annotations[annIncidentID]
	if !strings.HasPrefix(id, "inc-") {
		t.Fatalf("incident id annotation %q", id)
	}
	info := o.metrics.info.WithLabelValues("payment-api", "PaymentApiHighErrorRate", "escalated", id, "DB auth failed !channel after rotation")
	if v := testutil.ToFloat64(info); v != 1 {
		t.Fatalf("incident info = %v", v)
	}
	if n := testutil.CollectAndCount(o.metrics.info); n != 1 {
		t.Fatalf("%d info series, want 1", n)
	}
}

func TestLabelText(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"a\n\tb  <!here> & c", 50, "a b !here c"},
		{"ééééé", 5, "ééééé"},
		{"éééééé", 5, "éééé…"},
	} {
		if got := labelText(c.in, c.n); got != c.want {
			t.Errorf("labelText(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}
