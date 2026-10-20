# Authorization policy for the MCP server.
#
# Every MCP request (list, tool call, resource read, prompt) is evaluated here
# before any handler runs. The LLM never decides what it may do; this file
# does. It is embedded in the server binary and also served to the agent as
# platform://policy/mcp, so the agent can see its own boundaries.
#
# Input:
#   caller  Kubernetes identity from TokenReview, e.g.
#           "system:serviceaccount:sre-agent:sre-agent"
#   method  MCP method, e.g. "tools/call", "resources/read"
#   name    tool or prompt name, or resource URI
#   args    tool arguments (object), or prompt arguments
#
# Output (data.mcp.authz.decision):
#   {"allowed": bool, "reason": string, ["required_path": string]}
package mcp.authz

# Who may use the server at all.
callers := {"system:serviceaccount:sre-agent:sre-agent"}

# Services the agent may investigate and propose changes for.
services := {"payment-api"}

read_tools := {
	"get_incident_context",
	"get_service_health",
	"get_metrics",
	"get_error_patterns",
	"get_recent_logs",
	"get_recent_deployments",
	"get_git_changes",
}

# Tools that would change production directly. They exist only so the refusal
# is visible; the approved path is a pull request.
mutation_tools := {"patch_deployment"}

prompts := {"investigate_incident", "propose_remediation", "verify_recovery"}

list_methods := {"tools/list", "resources/list", "resources/templates/list", "prompts/list"}

args := object.get(input, "args", {})

service := object.get(args, "service", "")

decision := {"allowed": false, "reason": sprintf("caller %q is not authorised to use this server", [input.caller])} if {
	not input.caller in callers
} else := {"allowed": true, "reason": "listing is allowed for authorised callers"} if {
	input.method in list_methods
} else := {
	"allowed": false,
	"reason": "Direct production mutation is prohibited.",
	"required_path": "create_pull_request",
} if {
	input.method == "tools/call"
	input.name in mutation_tools
} else := {"allowed": true, "reason": sprintf("read-only tool on in-scope service %q", [service])} if {
	input.method == "tools/call"
	input.name in read_tools
	service in services
} else := {"allowed": false, "reason": sprintf("service %q is not in scope", [service])} if {
	input.method == "tools/call"
	input.name in read_tools
} else := {"allowed": true, "reason": "reading the status of an agent pull request"} if {
	input.method == "tools/call"
	input.name == "get_pull_request"
	object.get(args, "number", 0) > 0
	valid_revision(object.get(args, "deployed_revision", ""))
} else := pr_decision if {
	input.method == "tools/call"
	input.name == "create_pull_request"
} else := {"allowed": true, "reason": "platform documentation is readable"} if {
	input.method == "resources/read"
	startswith(input.name, "platform://")
} else := {"allowed": true, "reason": "workflow prompt"} if {
	input.method == "prompts/get"
	input.name in prompts
} else := {"allowed": false, "reason": sprintf("%s %q is not permitted", [input.method, input.name])}

# deployed_revision, when given, is a commit SHA and nothing else.
valid_revision("")

valid_revision(rev) if regex.match(`^[0-9a-f]{7,40}$`, rev)

# --- create_pull_request ----------------------------------------------------

files := object.get(args, "files", [])

pr_violations contains "at least one file is required" if count(files) == 0

pr_violations contains "branch must start with agent/" if {
	not startswith(object.get(args, "branch", ""), "agent/")
}

pr_violations contains sprintf("path %q is outside gitops/workloads/", [f.path]) if {
	some f in files
	not startswith(f.path, "gitops/workloads/")
}

pr_violations contains sprintf("path %q must not contain '..'", [f.path]) if {
	some f in files
	contains(f.path, "..")
}

# Secrets never travel through Git, and never through the agent.
pr_violations contains sprintf("path %q would add a Kubernetes Secret", [f.path]) if {
	some f in files
	regex.match(`(?m)^kind:\s*Secret\s*$`, object.get(f, "content", ""))
}

pr_violations contains sprintf("path %q is not part of an in-scope service", [f.path]) if {
	some f in files
	parts := split(f.path, "/")
	count(parts) >= 4
	not parts[3] in services
}

pr_decision := {"allowed": true, "reason": "pull request within policy; requires CI and human approval to merge"} if {
	count(pr_violations) == 0
}

pr_decision := {"allowed": false, "reason": concat("; ", sort(pr_violations))} if {
	count(pr_violations) > 0
}
