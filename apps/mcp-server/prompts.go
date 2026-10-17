package main

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"text/template"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Prompts encode the platform's incident workflow (doc.md §13–15) so every
// investigation follows the same evidence-first, change-through-Git path.
//
//go:embed prompts/*.md
var promptFiles embed.FS

type promptDef struct {
	name, title, description string
	args                     []*mcp.PromptArgument
}

var promptDefs = []promptDef{
	{"investigate_incident", "Investigate an incident",
		"Evidence-first investigation of an alert, ending in a hypothesis with evidence.",
		[]*mcp.PromptArgument{
			{Name: "service", Description: "affected service, e.g. payment-api", Required: true},
			{Name: "alert", Description: "alert name, e.g. PaymentApiHighErrorRate", Required: true},
		}},
	{"propose_remediation", "Propose a remediation",
		"Turn a confirmed hypothesis into a minimal pull request, or escalate when Git is the wrong path.",
		[]*mcp.PromptArgument{
			{Name: "service", Description: "affected service", Required: true},
			{Name: "hypothesis", Description: "the confirmed root cause and its evidence", Required: true},
		}},
	{"verify_recovery", "Verify recovery",
		"Confirm a deployed fix actually resolved the incident before closing it.",
		[]*mcp.PromptArgument{
			{Name: "service", Description: "affected service", Required: true},
			{Name: "pull_request", Description: "URL of the merged fix", Required: false},
		}},
}

func addPrompts(s *mcp.Server) {
	for _, def := range promptDefs {
		tmpl := template.Must(template.New(def.name).Option("missingkey=zero").ParseFS(promptFiles, "prompts/"+def.name+".md"))
		s.AddPrompt(&mcp.Prompt{Name: def.name, Title: def.title, Description: def.description, Arguments: def.args},
			func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				for _, a := range def.args {
					if a.Required && req.Params.Arguments[a.Name] == "" {
						return nil, fmt.Errorf("argument %q is required", a.Name)
					}
				}
				var buf bytes.Buffer
				if err := tmpl.ExecuteTemplate(&buf, def.name+".md", req.Params.Arguments); err != nil {
					return nil, err
				}
				return &mcp.GetPromptResult{
					Description: def.description,
					Messages:    []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: buf.String()}}},
				}, nil
			})
	}
}
