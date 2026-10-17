package main

import (
	"context"
	"strings"
	"testing"
)

const agent = "system:serviceaccount:sre-agent:sre-agent"

func TestPolicyDecisions(t *testing.T) {
	p, err := newPolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	file := func(path, content string) map[string]any { return map[string]any{"path": path, "content": content} }
	goodPR := map[string]any{
		"branch": "agent/fix-db-host",
		"files":  []any{file("gitops/workloads/dev/payment-api/deployment.yaml", "kind: Deployment\n")},
	}
	cases := []struct {
		name       string
		in         policyInput
		allowed    bool
		reasonPart string
		required   string
	}{
		{"unknown caller", policyInput{Caller: "system:serviceaccount:default:default", Method: "tools/list"}, false, "not authorised", ""},
		{"empty caller", policyInput{Method: "tools/call", Name: "get_service_health", Args: map[string]any{"service": "payment-api"}}, false, "not authorised", ""},
		{"list tools", policyInput{Caller: agent, Method: "tools/list"}, true, "", ""},
		{"read tool in scope", policyInput{Caller: agent, Method: "tools/call", Name: "get_error_patterns", Args: map[string]any{"service": "payment-api"}}, true, "", ""},
		{"read tool out of scope", policyInput{Caller: agent, Method: "tools/call", Name: "get_recent_logs", Args: map[string]any{"service": "kube-system"}}, false, "not in scope", ""},
		{"read tool no service", policyInput{Caller: agent, Method: "tools/call", Name: "get_metrics"}, false, "not in scope", ""},
		{"patch refused with path", policyInput{Caller: agent, Method: "tools/call", Name: "patch_deployment", Args: map[string]any{"service": "payment-api"}}, false, "prohibited", "create_pull_request"},
		{"unknown tool", policyInput{Caller: agent, Method: "tools/call", Name: "exec_in_pod"}, false, "not permitted", ""},
		{"good PR", policyInput{Caller: agent, Method: "tools/call", Name: "create_pull_request", Args: goodPR}, true, "human approval", ""},
		{"PR to main", policyInput{Caller: agent, Method: "tools/call", Name: "create_pull_request", Args: map[string]any{"branch": "main", "files": goodPR["files"]}}, false, "agent/", ""},
		{"PR outside workloads", policyInput{Caller: agent, Method: "tools/call", Name: "create_pull_request", Args: map[string]any{
			"branch": "agent/x", "files": []any{file("terraform/main.tf", "")}}}, false, "outside gitops/workloads/", ""},
		{"PR path traversal", policyInput{Caller: agent, Method: "tools/call", Name: "create_pull_request", Args: map[string]any{
			"branch": "agent/x", "files": []any{file("gitops/workloads/dev/payment-api/../../../../terraform/x", "")}}}, false, "'..'", ""},
		{"PR adds a Secret", policyInput{Caller: agent, Method: "tools/call", Name: "create_pull_request", Args: map[string]any{
			"branch": "agent/x", "files": []any{file("gitops/workloads/dev/payment-api/s.yaml", "apiVersion: v1\nkind: Secret\n")}}}, false, "Secret", ""},
		{"PR other service", policyInput{Caller: agent, Method: "tools/call", Name: "create_pull_request", Args: map[string]any{
			"branch": "agent/x", "files": []any{file("gitops/workloads/dev/mcp-server/deployment.yaml", "")}}}, false, "not part of an in-scope service", ""},
		{"PR no files", policyInput{Caller: agent, Method: "tools/call", Name: "create_pull_request", Args: map[string]any{"branch": "agent/x"}}, false, "at least one file", ""},
		{"read agent PR", policyInput{Caller: agent, Method: "tools/call", Name: "get_pull_request", Args: map[string]any{"number": 7}}, true, "", ""},
		{"read PR without number", policyInput{Caller: agent, Method: "tools/call", Name: "get_pull_request"}, false, "not permitted", ""},
		{"platform resource", policyInput{Caller: agent, Method: "resources/read", Name: "platform://docs/architecture"}, true, "", ""},
		{"other resource", policyInput{Caller: agent, Method: "resources/read", Name: "file:///etc/passwd"}, false, "not permitted", ""},
		{"known prompt", policyInput{Caller: agent, Method: "prompts/get", Name: "investigate_incident"}, true, "", ""},
		{"unknown prompt", policyInput{Caller: agent, Method: "prompts/get", Name: "jailbreak"}, false, "not permitted", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := p.evaluate(context.Background(), tc.in)
			if d.Allowed != tc.allowed {
				t.Fatalf("allowed=%v, want %v (reason %q)", d.Allowed, tc.allowed, d.Reason)
			}
			if tc.reasonPart != "" && !strings.Contains(d.Reason, tc.reasonPart) {
				t.Errorf("reason %q does not contain %q", d.Reason, tc.reasonPart)
			}
			if d.RequiredPath != tc.required {
				t.Errorf("required_path=%q, want %q", d.RequiredPath, tc.required)
			}
		})
	}
}
