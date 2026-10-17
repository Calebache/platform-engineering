package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Convention: a service named X runs as Deployment X in namespace X, and its
// Argo CD Application is also named X.

var argoApplications = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}

const argoNamespace = "argocd"

type kubeBackend struct {
	client  kubernetes.Interface
	dynamic dynamic.Interface
}

type PodStatus struct {
	Name            string `json:"name"`
	Component       string `json:"component"`
	Phase           string `json:"phase"`
	Ready           bool   `json:"ready"`
	Restarts        int32  `json:"restarts"`
	LastTermination string `json:"last_termination,omitempty"`
	Age             string `json:"age"`
}

type DeploymentStatus struct {
	Desired    int32    `json:"desired"`
	Ready      int32    `json:"ready"`
	Updated    int32    `json:"updated"`
	Available  int32    `json:"available"`
	Conditions []string `json:"conditions,omitempty"`
}

type EventSummary struct {
	Object  string `json:"object"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Count   int32  `json:"count"`
	Last    string `json:"last_seen"`
}

type ArgoStatus struct {
	Sync         string         `json:"sync"`
	Health       string         `json:"health"`
	Revision     string         `json:"revision"`
	LastSyncedAt string         `json:"last_synced_at,omitempty"`
	History      []ArgoRevision `json:"history,omitempty"`
}

type ArgoRevision struct {
	Revision   string `json:"revision"`
	DeployedAt string `json:"deployed_at"`
}

type ServiceHealth struct {
	Service    string            `json:"service"`
	Deployment *DeploymentStatus `json:"deployment"`
	Pods       []PodStatus       `json:"pods,omitempty"`
	Warnings   []EventSummary    `json:"recent_warning_events,omitempty"`
	ArgoCD     *ArgoStatus       `json:"argocd,omitempty"`
}

func (k *kubeBackend) health(ctx context.Context, svc string) (*ServiceHealth, error) {
	dep, err := k.client.AppsV1().Deployments(svc).Get(ctx, svc, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get deployment %s/%s: %w", svc, svc, err)
	}
	h := &ServiceHealth{Service: svc, Deployment: deploymentStatus(dep)}

	pods, err := k.client.CoreV1().Pods(svc).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	for _, p := range pods.Items {
		h.Pods = append(h.Pods, podStatus(&p))
	}
	sort.Slice(h.Pods, func(i, j int) bool { return h.Pods[i].Name < h.Pods[j].Name })

	events, err := k.client.CoreV1().Events(svc).List(ctx, metav1.ListOptions{FieldSelector: "type=Warning"})
	if err == nil {
		h.Warnings = warningSummaries(events.Items, 10)
	}
	h.ArgoCD, _ = k.argoStatus(ctx, svc) // optional: best effort
	return h, nil
}

func deploymentStatus(d *appsv1.Deployment) *DeploymentStatus {
	s := &DeploymentStatus{
		Ready: d.Status.ReadyReplicas, Updated: d.Status.UpdatedReplicas, Available: d.Status.AvailableReplicas,
	}
	if d.Spec.Replicas != nil {
		s.Desired = *d.Spec.Replicas
	}
	for _, c := range d.Status.Conditions {
		s.Conditions = append(s.Conditions, fmt.Sprintf("%s=%s (%s)", c.Type, c.Status, c.Reason))
	}
	return s
}

func podStatus(p *corev1.Pod) PodStatus {
	ps := PodStatus{
		Name:      p.Name,
		Component: p.Labels["app.kubernetes.io/name"],
		Phase:     string(p.Status.Phase),
		Age:       time.Since(p.CreationTimestamp.Time).Round(time.Second).String(),
	}
	ready := len(p.Status.ContainerStatuses) > 0
	for _, c := range p.Status.ContainerStatuses {
		ready = ready && c.Ready
		ps.Restarts += c.RestartCount
		if t := c.LastTerminationState.Terminated; t != nil {
			ps.LastTermination = fmt.Sprintf("%s (exit %d) at %s", t.Reason, t.ExitCode, t.FinishedAt.UTC().Format(time.RFC3339))
		}
	}
	ps.Ready = ready
	return ps
}

func warningSummaries(events []corev1.Event, limit int) []EventSummary {
	sort.Slice(events, func(i, j int) bool { return lastSeen(events[i]).After(lastSeen(events[j])) })
	var out []EventSummary
	for _, e := range events {
		if len(out) == limit {
			break
		}
		out = append(out, EventSummary{
			Object:  e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name,
			Reason:  e.Reason,
			Message: redact(truncate(e.Message, 300)),
			Count:   e.Count,
			Last:    lastSeen(e).UTC().Format(time.RFC3339),
		})
	}
	return out
}

func lastSeen(e corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

func (k *kubeBackend) argoStatus(ctx context.Context, svc string) (*ArgoStatus, error) {
	app, err := k.dynamic.Resource(argoApplications).Namespace(argoNamespace).Get(ctx, svc, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	s := &ArgoStatus{}
	s.Sync, _, _ = unstructured.NestedString(app.Object, "status", "sync", "status")
	s.Health, _, _ = unstructured.NestedString(app.Object, "status", "health", "status")
	s.Revision, _, _ = unstructured.NestedString(app.Object, "status", "sync", "revision")
	s.LastSyncedAt, _, _ = unstructured.NestedString(app.Object, "status", "operationState", "finishedAt")
	history, _, _ := unstructured.NestedSlice(app.Object, "status", "history")
	for i := len(history) - 1; i >= 0 && len(s.History) < 5; i-- { // newest first
		h, ok := history[i].(map[string]any)
		if !ok {
			continue
		}
		rev, _ := h["revision"].(string)
		at, _ := h["deployedAt"].(string)
		s.History = append(s.History, ArgoRevision{Revision: rev, DeployedAt: at})
	}
	return s, nil
}

// --- Rollout history ----------------------------------------------------

type Revision struct {
	Revision   int               `json:"revision"`
	ReplicaSet string            `json:"replicaset"`
	CreatedAt  string            `json:"created_at"`
	Replicas   int32             `json:"replicas"`
	Images     []string          `json:"images,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Changes    []string          `json:"changes_from_previous,omitempty"`
}

// rollouts reconstructs the Deployment's revision history from its
// ReplicaSets, including which images and env vars changed between them.
// Env values from Secrets are shown as references, never resolved.
func (k *kubeBackend) rollouts(ctx context.Context, svc string, limit int) ([]Revision, error) {
	dep, err := k.client.AppsV1().Deployments(svc).Get(ctx, svc, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get deployment: %w", err)
	}
	sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return nil, err
	}
	rsList, err := k.client.AppsV1().ReplicaSets(svc).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, fmt.Errorf("list replicasets: %w", err)
	}
	var revs []Revision
	for _, rs := range rsList.Items {
		if !ownedBy(rs.OwnerReferences, dep.UID) {
			continue
		}
		n, _ := strconv.Atoi(rs.Annotations["deployment.kubernetes.io/revision"])
		r := Revision{
			Revision: n, ReplicaSet: rs.Name,
			CreatedAt: rs.CreationTimestamp.UTC().Format(time.RFC3339),
			Replicas:  rs.Status.Replicas,
			Env:       map[string]string{},
		}
		for _, c := range rs.Spec.Template.Spec.Containers {
			r.Images = append(r.Images, c.Image)
			for _, e := range c.Env {
				r.Env[e.Name] = envValue(e)
			}
		}
		revs = append(revs, r)
	}
	sort.Slice(revs, func(i, j int) bool { return revs[i].Revision < revs[j].Revision })
	for i := 1; i < len(revs); i++ {
		revs[i].Changes = diffRevisions(revs[i-1], revs[i])
	}
	// newest first, capped
	for i, j := 0, len(revs)-1; i < j; i, j = i+1, j-1 {
		revs[i], revs[j] = revs[j], revs[i]
	}
	if len(revs) > limit {
		revs = revs[:limit]
	}
	return revs, nil
}

func ownedBy(refs []metav1.OwnerReference, uid any) bool {
	for _, r := range refs {
		if r.UID == uid {
			return true
		}
	}
	return false
}

func envValue(e corev1.EnvVar) string {
	if e.ValueFrom != nil {
		switch {
		case e.ValueFrom.SecretKeyRef != nil:
			return fmt.Sprintf("<secret %s/%s>", e.ValueFrom.SecretKeyRef.Name, e.ValueFrom.SecretKeyRef.Key)
		case e.ValueFrom.ConfigMapKeyRef != nil:
			return fmt.Sprintf("<configmap %s/%s>", e.ValueFrom.ConfigMapKeyRef.Name, e.ValueFrom.ConfigMapKeyRef.Key)
		default:
			return "<from field>"
		}
	}
	return redact(e.Value)
}

func diffRevisions(prev, cur Revision) []string {
	var out []string
	if strings.Join(prev.Images, ",") != strings.Join(cur.Images, ",") {
		out = append(out, fmt.Sprintf("image: %s -> %s", strings.Join(prev.Images, ","), strings.Join(cur.Images, ",")))
	}
	keys := map[string]bool{}
	for k := range prev.Env {
		keys[k] = true
	}
	for k := range cur.Env {
		keys[k] = true
	}
	var names []string
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		a, inPrev := prev.Env[k]
		b, inCur := cur.Env[k]
		switch {
		case !inPrev:
			out = append(out, fmt.Sprintf("env %s added: %q", k, b))
		case !inCur:
			out = append(out, fmt.Sprintf("env %s removed (was %q)", k, a))
		case a != b:
			out = append(out, fmt.Sprintf("env %s: %q -> %q", k, a, b))
		}
	}
	return out
}

// --- Logs -----------------------------------------------------------------

type LogLine struct {
	Pod  string `json:"pod"`
	Time string `json:"time"`
	Text string `json:"text"`
}

// logs returns recent lines from every pod of the service's Deployment,
// redacted, oldest first. perPod caps how much each pod contributes.
func (k *kubeBackend) logs(ctx context.Context, svc string, since time.Duration, perPod int64) ([]LogLine, error) {
	dep, err := k.client.AppsV1().Deployments(svc).Get(ctx, svc, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get deployment: %w", err)
	}
	sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return nil, err
	}
	pods, err := k.client.CoreV1().Pods(svc).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	sinceSec := int64(since.Seconds())
	limitBytes := int64(1 << 20)
	var lines []LogLine
	for _, p := range pods.Items {
		stream, err := k.client.CoreV1().Pods(svc).GetLogs(p.Name, &corev1.PodLogOptions{
			SinceSeconds: &sinceSec, TailLines: &perPod, LimitBytes: &limitBytes, Timestamps: true,
		}).Stream(ctx)
		if err != nil {
			continue // a pod that's starting or gone shouldn't fail the whole call
		}
		lines = append(lines, readLogLines(stream, p.Name)...)
		_ = stream.Close()
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].Time < lines[j].Time })
	return lines, nil
}

func readLogLines(r io.Reader, pod string) []LogLine {
	var out []LogLine
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		ts, text, _ := strings.Cut(sc.Text(), " ") // "--timestamps" prefixes RFC3339Nano
		out = append(out, LogLine{Pod: pod, Time: ts, Text: redact(text)})
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
