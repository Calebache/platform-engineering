package main

import rego.v1

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
