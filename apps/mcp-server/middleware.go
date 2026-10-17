package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// governed returns the receiving middleware every MCP request passes through:
// identify the caller, ask the policy, record the decision, and only then let
// the handler run. Handlers never see a request the policy hasn't allowed.
func governed(p *policy, m *metrics, logger *slog.Logger) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if !isGoverned(method) {
				return next(ctx, method, req)
			}
			start := time.Now()
			in := policyInput{Method: method}
			caller, err := callerFrom(req)
			if err == nil {
				in.Caller = caller
			}
			in.Name, in.Args = describe(req)

			d := p.evaluate(ctx, in)
			if err != nil { // no identity: deny regardless of policy
				d = decision{Reason: err.Error()}
			}

			var result mcp.Result
			var callErr error
			outcome := "denied"
			if d.Allowed {
				result, callErr = next(ctx, method, req)
				outcome = "allowed"
				if callErr != nil {
					outcome = "error"
				} else if r, ok := result.(*mcp.CallToolResult); ok && r.IsError {
					outcome = "tool_error"
				}
			} else {
				result, callErr = denial(method, d)
			}

			m.calls.WithLabelValues(method, metricName(method, in.Name), outcome).Inc()
			m.duration.WithLabelValues(method).Observe(time.Since(start).Seconds())
			attrs := []any{
				"caller", in.Caller, "method", method, "name", in.Name,
				"decision", outcome, "reason", d.Reason,
				"duration_ms", time.Since(start).Milliseconds(),
			}
			if len(in.Args) > 0 {
				attrs = append(attrs, "args", auditArgs(in.Args))
			}
			if d.RequiredPath != "" {
				attrs = append(attrs, "required_path", d.RequiredPath)
			}
			if callErr != nil {
				attrs = append(attrs, "error", callErr.Error())
			}
			logger.Info("mcp audit", attrs...)
			return result, callErr
		}
	}
}

func isGoverned(method string) bool {
	switch method {
	case "tools/call", "tools/list", "resources/read", "resources/list",
		"resources/templates/list", "prompts/get", "prompts/list":
		return true
	}
	return false
}

// describe extracts what the policy needs to see from each request type.
func describe(req mcp.Request) (string, map[string]any) {
	switch r := req.(type) {
	case *mcp.CallToolRequest:
		args := map[string]any{}
		if len(r.Params.Arguments) > 0 {
			_ = json.Unmarshal(r.Params.Arguments, &args)
		}
		return r.Params.Name, args
	case *mcp.ReadResourceRequest:
		return r.Params.URI, nil
	case *mcp.GetPromptRequest:
		args := map[string]any{}
		for k, v := range r.Params.Arguments {
			args[k] = v
		}
		return r.Params.Name, args
	}
	return "", nil
}

var errDenied = errors.New("denied by policy")

// denial shapes a refusal. For tools it's a normal tool result carrying the
// structured decision, so the agent can read "required_path" and change
// course; for other methods it's a protocol error.
func denial(method string, d decision) (mcp.Result, error) {
	if method == "tools/call" {
		body, _ := json.Marshal(d)
		return &mcp.CallToolResult{
			IsError:           true,
			Content:           []mcp.Content{&mcp.TextContent{Text: string(body)}},
			StructuredContent: d,
		}, nil
	}
	return nil, errors.Join(errDenied, errors.New(d.Reason))
}

// auditArgs keeps the audit log useful without copying whole file bodies
// from create_pull_request into it.
func auditArgs(args map[string]any) map[string]any {
	out := make(map[string]any, len(args))
	for k, v := range args {
		if k == "files" {
			if files, ok := v.([]any); ok {
				paths := make([]string, 0, len(files))
				for _, f := range files {
					if fm, ok := f.(map[string]any); ok {
						if p, ok := fm["path"].(string); ok {
							paths = append(paths, p)
						}
					}
				}
				out["files"] = paths
				continue
			}
		}
		if s, ok := v.(string); ok && len(s) > 300 {
			v = s[:300] + "…"
		}
		out[k] = v
	}
	return out
}

// metricName keeps label cardinality bounded (resource URIs are fine, they
// come from a fixed catalogue; anything else unexpected collapses).
func metricName(method, name string) string {
	if strings.HasSuffix(method, "/list") {
		return ""
	}
	if len(name) > 80 {
		return "other"
	}
	return name
}
