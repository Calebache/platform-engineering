# Secrets reach the cluster only through External Secrets (Secrets Manager is
# the source of truth), so a Secret in Git is almost always a leaked
# credential. The one exception is an Argo CD repository entry that carries
# no credential at all (e.g. the public Envoy Gateway OCI registry).
package main

import rego.v1

credential_keys := {"password", "username", "sshPrivateKey", "githubAppPrivateKey", "tlsClientCertKey", "bearerToken"}

deny contains msg if {
	input.kind == "Secret"
	not credential_free_repo_secret
	msg := sprintf("Secret/%s: Secrets don't belong in Git; store the value in Secrets Manager and sync it with an ExternalSecret", [input.metadata.name])
}

credential_free_repo_secret if {
	input.metadata.labels["argocd.argoproj.io/secret-type"] in {"repository", "repo-creds"}
	not has_credential
}

has_credential if {
	some k, _ in object.union(object.get(input, "data", {}), object.get(input, "stringData", {}))
	k in credential_keys
}
