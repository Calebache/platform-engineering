package main

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/open-policy-agent/opa/v1/rego"
)

//go:embed policy/mcp.rego
var policySource string

// decision is the policy's verdict, returned verbatim to the agent when a
// request is denied.
type decision struct {
	Allowed      bool   `json:"allowed"`
	Reason       string `json:"reason"`
	RequiredPath string `json:"required_path,omitempty"`
}

type policyInput struct {
	Caller string         `json:"caller"`
	Method string         `json:"method"`
	Name   string         `json:"name"`
	Args   map[string]any `json:"args"`
}

type policy struct {
	query rego.PreparedEvalQuery
}

func newPolicy(ctx context.Context) (*policy, error) {
	q, err := rego.New(
		rego.Query("data.mcp.authz.decision"),
		rego.Module("policy/mcp.rego", policySource),
	).PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("compile policy: %w", err)
	}
	return &policy{query: q}, nil
}

// evaluate fails closed: any evaluation problem is a denial.
func (p *policy) evaluate(ctx context.Context, in policyInput) decision {
	if in.Args == nil {
		in.Args = map[string]any{}
	}
	rs, err := p.query.Eval(ctx, rego.EvalInput(in))
	if err != nil {
		return decision{Reason: "policy evaluation failed: " + err.Error()}
	}
	if len(rs) == 0 || len(rs[0].Expressions) == 0 {
		return decision{Reason: "policy returned no decision"}
	}
	m, ok := rs[0].Expressions[0].Value.(map[string]any)
	if !ok {
		return decision{Reason: "policy returned a malformed decision"}
	}
	d := decision{}
	d.Allowed, _ = m["allowed"].(bool)
	d.Reason, _ = m["reason"].(string)
	d.RequiredPath, _ = m["required_path"].(string)
	return d
}
