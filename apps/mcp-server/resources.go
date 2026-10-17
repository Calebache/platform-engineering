package main

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Resources are read live from the repository through the GitHub App, so the
// agent always sees the current, reviewed version and there is one source of
// truth. The catalogue is fixed: the agent can't ask for arbitrary paths.
type resourceEntry struct {
	uri, path, name, title, description, mime string
}

var resourceCatalogue = []resourceEntry{
	{"platform://docs/architecture", "docs/architecture.md", "architecture", "Platform architecture",
		"As-built architecture: ownership boundaries, network, identity, secrets, components and the agent loop.", "text/markdown"},
	{"platform://services/payment-api/readme", "apps/payment-api/README.md", "payment-api-readme", "payment-api README",
		"API, configuration, incident-relevant behaviour, metrics, logs and alerts of payment-api.", "text/markdown"},
	{"platform://services/payment-api/schema", "apps/payment-api/schema.sql", "payment-api-schema", "payment-api database schema",
		"The Postgres schema payment-api applies at startup (single source of truth).", "application/sql"},
	{"platform://services/payment-api/runbook", "docs/runbooks/payment-api.md", "payment-api-runbook", "payment-api runbook",
		"Triage steps, known causes mapped to log errors, remediation rules and how to verify recovery.", "text/markdown"},
	{"platform://policy/mcp", "apps/mcp-server/policy/mcp.rego", "mcp-policy", "MCP authorization policy",
		"The OPA policy this server enforces on every request: what you may and may not do.", "text/plain"},
}

const maxResourceBytes = 200 << 10

func (d *deps) addResources(s *mcp.Server) {
	for _, e := range resourceCatalogue {
		s.AddResource(&mcp.Resource{
			URI: e.uri, Name: e.name, Title: e.title, Description: e.description, MIMEType: e.mime,
		}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			body, err := d.git.readFile(ctx, e.path)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", e.uri, err)
			}
			if len(body) > maxResourceBytes {
				body = body[:maxResourceBytes] + "\n…(truncated)"
			}
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: e.uri, MIMEType: e.mime, Text: body}}}, nil
		})
	}
}
