package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// modelClient is the one Messages API call the loop makes; the Bedrock Mantle
// client satisfies it, and tests substitute scripted responses.
type modelClient interface {
	New(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error)
}

var (
	errRefusal     = errors.New("model declined the request")
	errLimit       = errors.New("incident limit reached")
	errNoSubmit    = errors.New("model stopped without submitting a result")
	errPRAlreadyOn = errors.New("a pull request has already been opened for this incident")
)

// limits bound one incident. They're enforced here, independently of the
// model's own judgement and of the MCP server's policy.
type limits struct {
	maxToolCalls   int // across all phases
	maxModelCalls  int
	maxOutputTotal int64 // output tokens across all phases
}

// localTool ends a phase: the model reports its result by calling it, so the
// result is structured, not parsed out of prose.
type localTool struct {
	name, description string
	schema            map[string]any
	required          []string
}

type phaseResult struct {
	tool  string
	input json.RawMessage
}

// incidentRun carries what all phases of one incident share.
type incidentRun struct {
	model   modelClient
	mcp     toolSession
	audit   *auditor
	logger  *slog.Logger
	limits  limits
	modelID string
	system  string

	mcpTools  []anthropic.ToolUnionParam
	toolCalls int
	prOpened  *pullRequestRef
}

type pullRequestRef struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Branch string `json:"branch"`
}

// phase runs one conversation: a prompt, the MCP tools allowed in this phase,
// and the local tools that finish it. Returns which local tool was called and
// with what input.
func (r *incidentRun) phase(ctx context.Context, name, prompt string, allowed map[string]bool, finish []localTool) (*phaseResult, error) {
	tools := r.toolsFor(allowed, finish)
	finishNames := map[string]bool{}
	for _, f := range finish {
		finishNames[f.name] = true
	}
	messages := []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))}
	nudged := false

	for {
		if err := r.checkBudget(); err != nil {
			return nil, err
		}
		resp, err := r.model.New(ctx, anthropic.MessageNewParams{
			Model:     anthropic.Model(r.modelID),
			MaxTokens: 16000,
			// Thinking is always on for Claude Opus 5.5; effort sets its depth.
			OutputConfig: anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffortHigh},
			// Explicit breakpoint (Bedrock has no automatic caching): tools and
			// system prompt are identical across turns and phases.
			System:   []anthropic.TextBlockParam{{Text: r.system, CacheControl: anthropic.NewCacheControlEphemeralParam()}},
			Tools:    tools,
			Messages: messages,
		})
		if err != nil {
			return nil, fmt.Errorf("%s: model call: %w", name, err)
		}
		r.audit.update(func(a *AuditRecord) {
			a.Usage.ModelCalls++
			a.Usage.InputTokens += resp.Usage.InputTokens
			a.Usage.OutputTokens += resp.Usage.OutputTokens
			a.Usage.CacheRead += resp.Usage.CacheReadInputTokens
		})

		switch resp.StopReason {
		case anthropic.StopReasonRefusal:
			return nil, fmt.Errorf("%w (%s): %s", errRefusal, resp.StopDetails.Category, resp.StopDetails.Explanation)
		case anthropic.StopReasonModelContextWindowExceeded:
			return nil, fmt.Errorf("%s: %w: context window exceeded", name, errLimit)
		}

		// Append-only history: the assistant turn goes in unchanged (it carries
		// thinking blocks that must be replayed as-is).
		messages = append(messages, resp.ToParam())

		var results []anthropic.ContentBlockParamUnion
		for _, block := range resp.Content {
			use, ok := block.AsAny().(anthropic.ToolUseBlock)
			if !ok {
				continue
			}
			raw := json.RawMessage(use.JSON.Input.Raw())
			if finishNames[use.Name] {
				r.audit.recordCall(ToolCall{Phase: name, Tool: use.Name, Decision: "local"})
				return &phaseResult{tool: use.Name, input: raw}, nil
			}
			text, isErr := r.callMCP(ctx, name, use.Name, raw)
			results = append(results, anthropic.NewToolResultBlock(use.ID, text, isErr))
		}

		switch {
		case len(results) > 0:
			messages = append(messages, anthropic.NewUserMessage(results...))
		case resp.StopReason == anthropic.StopReasonMaxTokens:
			messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock("Continue.")))
		case !nudged:
			nudged = true
			messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(
				"Finish this step by calling one of: "+strings.Join(keys(finishNames), ", ")+".")))
		default:
			return nil, fmt.Errorf("%s: %w", name, errNoSubmit)
		}
	}
}

func (r *incidentRun) checkBudget() error {
	rec := r.audit.snapshot()
	switch {
	case r.limits.maxModelCalls > 0 && rec.Usage.ModelCalls >= r.limits.maxModelCalls:
		return fmt.Errorf("%w: %d model calls", errLimit, rec.Usage.ModelCalls)
	case r.limits.maxOutputTotal > 0 && rec.Usage.OutputTokens >= r.limits.maxOutputTotal:
		return fmt.Errorf("%w: %d output tokens", errLimit, rec.Usage.OutputTokens)
	}
	return nil
}

// callMCP runs one tool through the MCP server, with the agent's own guards
// first. Every call, allowed or not, lands in the audit record.
func (r *incidentRun) callMCP(ctx context.Context, phase, tool string, raw json.RawMessage) (string, bool) {
	start := time.Now()
	record := func(decision, reason string) {
		r.audit.recordCall(ToolCall{
			Phase: phase, Tool: tool, Decision: decision, Reason: reason,
			Args: summariseArgs(raw), DurationMs: time.Since(start).Milliseconds(),
		})
	}

	r.toolCalls++
	if r.limits.maxToolCalls > 0 && r.toolCalls > r.limits.maxToolCalls {
		record("error", "tool call limit")
		return "Tool call limit for this incident reached. Submit your result now with what you have.", true
	}
	if tool == "create_pull_request" && r.prOpened != nil {
		record("error", errPRAlreadyOn.Error())
		return errPRAlreadyOn.Error() + ": " + r.prOpened.URL, true
	}

	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		record("error", "invalid arguments")
		return "Invalid JSON arguments: " + err.Error(), true
	}
	res, err := r.mcp.callTool(ctx, tool, args)
	if err != nil {
		record("error", err.Error())
		return "Tool call failed: " + err.Error(), true
	}
	text := truncate(resultText(res), 24<<10)
	if reason, denied := denialReason(res); denied {
		record("denied", reason)
		r.logger.Warn("tool denied by policy", "phase", phase, "tool", tool, "reason", reason)
		return text, true
	}
	if res.IsError {
		record("error", truncate(text, 300))
		return text, true
	}
	record("allowed", "")
	if tool == "create_pull_request" {
		var pr pullRequestRef
		if json.Unmarshal([]byte(text), &pr) == nil && pr.Number > 0 {
			r.prOpened = &pr
		}
	}
	return text, false
}

func (r *incidentRun) toolsFor(allowed map[string]bool, finish []localTool) []anthropic.ToolUnionParam {
	var out []anthropic.ToolUnionParam
	for _, t := range r.mcpTools {
		if allowed[t.OfTool.Name] {
			out = append(out, t)
		}
	}
	for _, f := range finish {
		t := anthropic.ToolParam{
			Name:        f.name,
			Description: anthropic.String(f.description),
			InputSchema: anthropic.ToolInputSchemaParam{Properties: f.schema, Required: f.required},
		}
		out = append(out, anthropic.ToolUnionParam{OfTool: &t})
	}
	return out
}

// mcpToolsToClaude converts the MCP server's tool list into Claude tool
// definitions. The schemas come from the server, so the agent never has a
// tool the gateway doesn't know about.
func mcpToolsToClaude(tools []*mcpToolLike) []anthropic.ToolUnionParam {
	var out []anthropic.ToolUnionParam
	for _, t := range tools {
		schema := map[string]any{}
		if b, err := json.Marshal(t.InputSchema); err == nil {
			_ = json.Unmarshal(b, &schema)
		}
		var required []string
		if req, ok := schema["required"].([]any); ok {
			for _, r := range req {
				if s, ok := r.(string); ok {
					required = append(required, s)
				}
			}
		}
		p := anthropic.ToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: anthropic.ToolInputSchemaParam{Properties: schema["properties"], Required: required},
		}
		out = append(out, anthropic.ToolUnionParam{OfTool: &p})
	}
	return out
}

type mcpToolLike struct {
	Name, Description string
	InputSchema       any
}

func summariseArgs(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return truncate(string(raw), 200)
	}
	if files, ok := m["files"].([]any); ok { // PR bodies: keep paths, drop content
		var paths []string
		for _, f := range files {
			if fm, ok := f.(map[string]any); ok {
				paths = append(paths, fmt.Sprint(fm["path"]))
			}
		}
		m["files"] = paths
	}
	delete(m, "body")
	b, _ := json.Marshal(m)
	return truncate(string(b), 300)
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}
