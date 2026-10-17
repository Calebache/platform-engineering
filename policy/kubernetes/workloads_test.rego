package main

import rego.v1

good_container := {
	"name": "app",
	"image": "103544553431.dkr.ecr.ca-central-1.amazonaws.com/payment-api:66b2f83008a0",
	"securityContext": {"allowPrivilegeEscalation": false, "capabilities": {"drop": ["ALL"]}},
	"resources": {"requests": {"memory": "64Mi"}, "limits": {"memory": "128Mi"}},
}

deployment(container) := {
	"kind": "Deployment",
	"metadata": {"name": "demo"},
	"spec": {"template": {"spec": {
		"securityContext": {"runAsNonRoot": true},
		"containers": [container],
	}}},
}

test_good_deployment_passes if {
	count(deny) == 0 with input as deployment(good_container)
}

# The doc.md §21 demo: a privileged container fails the policy check.
test_privileged_denied if {
	c := json.patch(good_container, [{"op": "add", "path": "/securityContext/privileged", "value": true}])
	some msg in deny with input as deployment(c)
	contains(msg, "privileged: true")
}

test_privilege_escalation_denied if {
	c := json.patch(good_container, [{"op": "remove", "path": "/securityContext/allowPrivilegeEscalation"}])
	some msg in deny with input as deployment(c)
	contains(msg, "allowPrivilegeEscalation")
}

test_root_denied if {
	d := json.patch(deployment(good_container), [{"op": "remove", "path": "/spec/template/spec/securityContext"}])
	some msg in deny with input as d
	contains(msg, "non-root")
}

test_container_cannot_override_non_root if {
	c := json.patch(good_container, [{"op": "add", "path": "/securityContext/runAsNonRoot", "value": false}])
	some msg in deny with input as deployment(c)
	contains(msg, "non-root")
}

test_host_network_denied if {
	d := json.patch(deployment(good_container), [{"op": "add", "path": "/spec/template/spec/hostNetwork", "value": true}])
	some msg in deny with input as d
	contains(msg, "hostNetwork")
}

test_latest_and_untagged_denied if {
	every image in ["nginx", "nginx:latest", "registry.example.com:5000/app"] {
		c := json.patch(good_container, [{"op": "replace", "path": "/image", "value": image}])
		some msg in deny with input as deployment(c)
		contains(msg, "must be pinned")
	}
}

test_digest_and_registry_port_allowed if {
	every image in ["nginx@sha256:0123", "registry.example.com:5000/app:1.2.3", "postgres:18.6-alpine3.24"] {
		c := json.patch(good_container, [{"op": "replace", "path": "/image", "value": image}])
		count(deny) == 0 with input as deployment(c)
	}
}

test_cronjob_checked if {
	c := json.patch(good_container, [{"op": "add", "path": "/securityContext/privileged", "value": true}])
	cron := {"kind": "CronJob", "metadata": {"name": "demo"}, "spec": {"jobTemplate": {"spec": {"template": {"spec": {
		"securityContext": {"runAsNonRoot": true},
		"containers": [c],
	}}}}}}
	some msg in deny with input as cron
	contains(msg, "privileged")
}

test_secret_denied if {
	some msg in deny with input as {"kind": "Secret", "metadata": {"name": "db"}, "stringData": {"password": "x"}}
	contains(msg, "ExternalSecret")
}

test_credential_free_repo_secret_allowed if {
	count(deny) == 0 with input as {
		"kind": "Secret",
		"metadata": {"name": "repo", "labels": {"argocd.argoproj.io/secret-type": "repository"}},
		"stringData": {"type": "helm", "url": "docker.io/envoyproxy", "enableOCI": "true"},
	}
}

test_repo_secret_with_credential_denied if {
	some msg in deny with input as {
		"kind": "Secret",
		"metadata": {"name": "repo", "labels": {"argocd.argoproj.io/secret-type": "repository"}},
		"stringData": {"url": "https://github.com/x/y", "password": "ghp_x"},
	}
	contains(msg, "ExternalSecret")
}
