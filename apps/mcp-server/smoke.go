package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// runSmoke exercises the server as a real client would, including the
// denials. Run it as a pod with the caller's ServiceAccount; it exits non-zero
// if any expectation fails.
func runSmoke(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("smoke", flag.ContinueOnError)
	endpoint := fs.String("endpoint", "http://mcp-server.mcp-server.svc/mcp", "MCP endpoint")
	tokenFile := fs.String("token-file", "/var/run/secrets/mcp/token", "projected ServiceAccount token (audience mcp-server)")
	service := fs.String("service", "payment-api", "service to investigate")
	if err := fs.Parse(args); err != nil {
		return err
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "mcp-smoke", Version: version}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   *endpoint,
		HTTPClient: &http.Client{Timeout: 60 * time.Second, Transport: bearerFromFile{path: *tokenFile}},
	}, nil)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer session.Close()

	var failures []string
	check := func(name string, ok bool, detail string) {
		status := "PASS"
		if !ok {
			status = "FAIL"
			failures = append(failures, name)
		}
		fmt.Printf("%s  %-45s %s\n", status, name, truncate(detail, 160))
	}

	tools, err := session.ListTools(ctx, nil)
	check("tools/list", err == nil && len(tools.Tools) == 9, fmt.Sprintf("%d tools, err=%v", lenTools(tools), err))
	res, err := session.ListResources(ctx, nil)
	check("resources/list", err == nil && len(res.Resources) == len(resourceCatalogue), fmt.Sprintf("err=%v", err))
	prompts, err := session.ListPrompts(ctx, nil)
	check("prompts/list", err == nil && len(prompts.Prompts) == len(promptDefs), fmt.Sprintf("err=%v", err))

	call := func(name string, args map[string]any) (*mcp.CallToolResult, string) {
		r, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			return nil, err.Error()
		}
		b, _ := json.Marshal(r.StructuredContent)
		return r, string(b)
	}

	r, body := call("get_incident_context", map[string]any{"service": *service})
	check("get_incident_context", r != nil && !r.IsError && strings.Contains(body, `"health"`), body)
	r, body = call("get_error_patterns", map[string]any{"service": *service, "window": "30m"})
	check("get_error_patterns", r != nil && !r.IsError, body)
	r, body = call("get_recent_deployments", map[string]any{"service": *service})
	check("get_recent_deployments", r != nil && !r.IsError && strings.Contains(body, `"rollouts"`), body)
	r, body = call("get_git_changes", map[string]any{"service": *service, "limit": 2})
	check("get_git_changes", r != nil && !r.IsError && strings.Contains(body, `"commits"`), body)

	rr, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "platform://services/" + *service + "/runbook"})
	check("resources/read runbook", err == nil && len(rr.Contents) == 1 && strings.Contains(rr.Contents[0].Text, "Known causes"), fmt.Sprintf("err=%v", err))
	gp, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "investigate_incident", Arguments: map[string]string{"service": *service, "alert": "PaymentApiHighErrorRate"}})
	check("prompts/get investigate_incident", err == nil && len(gp.Messages) == 1, fmt.Sprintf("err=%v", err))

	// The governance checks: these must be refused.
	r, body = call("patch_deployment", map[string]any{"service": *service, "patch": `{"spec":{"replicas":0}}`})
	check("DENY patch_deployment -> required_path", r != nil && r.IsError && strings.Contains(body, `"required_path":"create_pull_request"`), body)
	r, body = call("get_service_health", map[string]any{"service": "kube-system"})
	check("DENY out-of-scope service", r != nil && r.IsError && strings.Contains(body, "not in scope"), body)
	r, body = call("create_pull_request", map[string]any{
		"branch": "main", "title": "smoke", "body": "smoke",
		"files": []map[string]string{{"path": "terraform/main.tf", "content": "x"}},
	})
	check("DENY PR outside gitops/workloads", r != nil && r.IsError && strings.Contains(body, "outside gitops/workloads"), body)
	_, err = session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "file:///etc/passwd"})
	check("DENY arbitrary resource", err != nil, fmt.Sprintf("err=%v", err))

	if len(failures) > 0 {
		return fmt.Errorf("%d smoke check(s) failed: %s", len(failures), strings.Join(failures, ", "))
	}
	fmt.Println("all smoke checks passed")
	return nil
}

func lenTools(r *mcp.ListToolsResult) int {
	if r == nil {
		return 0
	}
	return len(r.Tools)
}

// bearerFromFile re-reads the projected token on every request, since the
// kubelet rotates it.
type bearerFromFile struct{ path string }

func (b bearerFromFile) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := os.ReadFile(b.path)
	if err != nil {
		return nil, errors.Join(errors.New("read token"), err)
	}
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	return http.DefaultTransport.RoundTrip(req)
}
