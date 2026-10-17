package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolSession is the slice of an MCP client session the worker uses.
type toolSession interface {
	listTools(ctx context.Context) ([]*mcp.Tool, error)
	callTool(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error)
	prompt(ctx context.Context, name string, args map[string]string) (string, error)
}

type mcpSession struct{ s *mcp.ClientSession }

// connectMCP authenticates with the projected ServiceAccount token (audience
// mcp-server), re-read on every request because the kubelet rotates it.
func connectMCP(ctx context.Context, endpoint, tokenFile string) (*mcpSession, func(), error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "sre-agent", Version: version}, nil)
	s, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Timeout: 90 * time.Second, Transport: bearerFromFile{path: tokenFile}},
	}, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to MCP server: %w", err)
	}
	return &mcpSession{s: s}, func() { _ = s.Close() }, nil
}

func (m *mcpSession) listTools(ctx context.Context) ([]*mcp.Tool, error) {
	var tools []*mcp.Tool
	for t, err := range m.s.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		tools = append(tools, t)
	}
	return tools, nil
}

func (m *mcpSession) callTool(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error) {
	return m.s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
}

func (m *mcpSession) prompt(ctx context.Context, name string, args map[string]string) (string, error) {
	r, err := m.s.GetPrompt(ctx, &mcp.GetPromptParams{Name: name, Arguments: args})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, msg := range r.Messages {
		if t, ok := msg.Content.(*mcp.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String(), nil
}

// resultText flattens an MCP tool result for the model: structured content
// when present (the MCP server returns JSON), text otherwise.
func resultText(r *mcp.CallToolResult) string {
	if r.StructuredContent != nil {
		if b, err := json.Marshal(r.StructuredContent); err == nil {
			return string(b)
		}
	}
	var parts []string
	for _, c := range r.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// denialReason extracts the policy's reason from a refused call, if it is one.
func denialReason(r *mcp.CallToolResult) (reason string, denied bool) {
	if !r.IsError {
		return "", false
	}
	var d struct {
		Allowed *bool  `json:"allowed"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(resultText(r)), &d); err == nil && d.Allowed != nil && !*d.Allowed {
		return d.Reason, true
	}
	return "", false
}

type bearerFromFile struct{ path string }

func (b bearerFromFile) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := os.ReadFile(b.path)
	if err != nil {
		return nil, errors.Join(errors.New("read MCP token"), err)
	}
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	return http.DefaultTransport.RoundTrip(req)
}
