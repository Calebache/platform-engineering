# Deterministic guardrails for every Kubernetes manifest in gitops/, run by
# Conftest in CI (doc.md §7). They hold for any change, a human's or the SRE
# agent's: the agent's PRs pass through exactly the same checks.
#
#   conftest test --policy policy/kubernetes $(git ls-files 'gitops/**/*.yaml')
#   conftest verify --policy policy/kubernetes     # the policy's own tests
package main

import rego.v1

workload_kinds := {"Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job"}

pod_spec := input.spec.template.spec if input.kind in workload_kinds

pod_spec := input.spec.jobTemplate.spec.template.spec if input.kind == "CronJob"

pod_spec := input.spec if input.kind == "Pod"

containers contains c if some c in pod_spec.containers

containers contains c if some c in pod_spec.initContainers

name := sprintf("%s/%s", [input.kind, input.metadata.name])

# --- Privilege ---------------------------------------------------------------

deny contains msg if {
	some c in containers
	c.securityContext.privileged == true
	msg := sprintf("%s: container %q sets securityContext.privileged: true; privileged containers are never allowed", [name, c.name])
}

deny contains msg if {
	some c in containers
	not c.securityContext.allowPrivilegeEscalation == false
	msg := sprintf("%s: container %q must set securityContext.allowPrivilegeEscalation: false", [name, c.name])
}

deny contains msg if {
	some c in containers
	not "ALL" in object.get(c, ["securityContext", "capabilities", "drop"], [])
	msg := sprintf("%s: container %q must drop all capabilities (securityContext.capabilities.drop: [\"ALL\"])", [name, c.name])
}

deny contains msg if {
	some c in containers
	some cap in object.get(c, ["securityContext", "capabilities", "add"], [])
	cap != "NET_BIND_SERVICE"
	msg := sprintf("%s: container %q adds capability %s", [name, c.name, cap])
}

deny contains msg if {
	some c in containers
	not runs_as_non_root(c)
	msg := sprintf("%s: container %q must run as non-root (runAsNonRoot: true on the pod or container)", [name, c.name])
}

runs_as_non_root(c) if c.securityContext.runAsNonRoot == true

runs_as_non_root(c) if {
	pod_spec.securityContext.runAsNonRoot == true
	not c.securityContext.runAsNonRoot == false
}

# --- Host access -------------------------------------------------------------

deny contains msg if {
	some field in ["hostNetwork", "hostPID", "hostIPC"]
	pod_spec[field] == true
	msg := sprintf("%s: %s: true is not allowed", [name, field])
}

deny contains msg if {
	some v in pod_spec.volumes
	v.hostPath
	msg := sprintf("%s: volume %q mounts a hostPath", [name, v.name])
}

# --- Images and resources ----------------------------------------------------

deny contains msg if {
	some c in containers
	not pinned(c.image)
	msg := sprintf("%s: container %q image %q must be pinned to a tag (not latest) or a digest", [name, c.name, c.image])
}

pinned(image) if contains(image, "@sha256:")

pinned(image) if {
	not contains(image, "@")
	parts := split(image, "/")
	last := parts[count(parts) - 1]
	contains(last, ":")
	not endswith(last, ":latest")
}

deny contains msg if {
	some c in containers
	some field in ["requests", "limits"]
	not c.resources[field].memory
	msg := sprintf("%s: container %q must set resources.%s.memory", [name, c.name, field])
}
