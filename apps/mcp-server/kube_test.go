package main

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func podSpec(dbHost string) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/name": "payment-api"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "payment-api", Image: "ecr/payment-api:abc",
			Env: []corev1.EnvVar{
				{Name: "DATABASE_HOST", Value: dbHost},
				{Name: "DATABASE_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "payment-api-db"}, Key: "password"}}},
			},
		}}},
	}
}

func TestRolloutsShowWhatChanged(t *testing.T) {
	two := int32(2)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "payment-api", Namespace: "payment-api", UID: "dep-uid"},
		Spec: appsv1.DeploymentSpec{Replicas: &two, Template: podSpec("wrong-host"),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "payment-api"}}},
	}
	rs := func(name, rev, host string) *appsv1.ReplicaSet {
		return &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "payment-api",
				Labels:          map[string]string{"app.kubernetes.io/name": "payment-api"},
				Annotations:     map[string]string{"deployment.kubernetes.io/revision": rev},
				OwnerReferences: []metav1.OwnerReference{{UID: "dep-uid"}}},
			Spec: appsv1.ReplicaSetSpec{Template: podSpec(host)},
		}
	}
	client := fake.NewClientset(dep,
		rs("payment-api-1", "1", "postgres.payment-api.svc.cluster.local"),
		rs("payment-api-2", "2", "wrong-host"),
	)
	k := &kubeBackend{client: client, dynamic: dynfake.NewSimpleDynamicClient(runtime.NewScheme())}

	revs, err := k.rollouts(context.Background(), "payment-api", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 2 || revs[0].Revision != 2 {
		t.Fatalf("unexpected revisions %+v", revs)
	}
	changes := strings.Join(revs[0].Changes, "; ")
	if !strings.Contains(changes, `env DATABASE_HOST: "postgres.payment-api.svc.cluster.local" -> "wrong-host"`) {
		t.Errorf("change not reported: %q", changes)
	}
	if got := revs[0].Env["DATABASE_PASSWORD"]; got != "<secret payment-api-db/password>" {
		t.Errorf("secret env must be a reference, got %q", got)
	}

	h, err := k.health(context.Background(), "payment-api")
	if err != nil {
		t.Fatal(err)
	}
	if h.Deployment.Desired != 2 || h.ArgoCD != nil {
		t.Errorf("health %+v", h)
	}
}
