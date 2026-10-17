package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// AuditRecord is one incident, end to end (doc.md §16). It is rewritten at
// every phase under the same key; the bucket is versioned, so each phase's
// version is kept.
type AuditRecord struct {
	ExecutionID string `json:"execution_id"`
	Agent       string `json:"agent"`
	AgentVer    string `json:"agent_version"`
	InitiatedBy string `json:"initiated_by"`
	Alert       string `json:"alert"`
	Service     string `json:"service"`
	Environment string `json:"environment"`
	Model       string `json:"model"`
	StartedAt   string `json:"started_at"`
	FinishedAt  string `json:"finished_at,omitempty"`
	Phase       string `json:"phase"`
	Outcome     string `json:"outcome"`

	ToolsUsed []string   `json:"tools_used"`
	ToolCalls []ToolCall `json:"tool_calls"`

	Hypothesis  string   `json:"hypothesis,omitempty"`
	Evidence    []string `json:"evidence,omitempty"`
	Confidence  string   `json:"confidence,omitempty"`
	Remediation string   `json:"remediation_path,omitempty"`
	// What the agent says should happen next; for an escalation, the steps
	// a human should take.
	Recommendation string `json:"recommendation,omitempty"`

	ProposedAction   string   `json:"proposed_action,omitempty"`
	PolicyResult     string   `json:"policy_result"`
	PolicyDenials    []string `json:"policy_denials,omitempty"`
	SecurityResult   string   `json:"security_result"`
	HumanApproval    *bool    `json:"human_approval"`
	ApprovedBy       string   `json:"approved_by,omitempty"`
	DeploymentResult string   `json:"deployment_result,omitempty"`
	ServiceHealth    string   `json:"service_health,omitempty"`
	Verification     []string `json:"verification,omitempty"`

	Usage Usage  `json:"usage"`
	Error string `json:"error,omitempty"`
}

type ToolCall struct {
	Phase      string `json:"phase"`
	Tool       string `json:"tool"`
	Decision   string `json:"decision"` // allowed | denied | error | local
	Reason     string `json:"reason,omitempty"`
	Args       string `json:"args,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

type Usage struct {
	ModelCalls   int   `json:"model_calls"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	CacheRead    int64 `json:"cache_read_input_tokens"`
}

type auditStore interface {
	put(ctx context.Context, key string, body []byte) error
}

type s3Store struct {
	client *s3.Client
	bucket string
}

func (s *s3Store) put(ctx context.Context, key string, body []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key),
		Body: bytes.NewReader(body), ContentType: aws.String("application/json"),
	})
	return err
}

// auditor owns the record and serialises writes to it.
type auditor struct {
	mu     sync.Mutex
	rec    *AuditRecord
	store  auditStore
	key    string
	logger interface {
		Error(msg string, args ...any)
	}
}

func newAuditor(store auditStore, rec *AuditRecord, logger interface{ Error(string, ...any) }) *auditor {
	t, _ := time.Parse(time.RFC3339, rec.StartedAt)
	return &auditor{
		store: store, rec: rec, logger: logger,
		key: fmt.Sprintf("audit/%s/%s.json", t.UTC().Format("2006/01/02"), rec.ExecutionID),
	}
}

func (a *auditor) update(f func(r *AuditRecord)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f(a.rec)
}

func (a *auditor) recordCall(c ToolCall) {
	a.update(func(r *AuditRecord) {
		r.ToolCalls = append(r.ToolCalls, c)
		if c.Decision == "denied" {
			r.PolicyDenials = append(r.PolicyDenials, c.Tool+": "+c.Reason)
		}
		if c.Decision != "local" && !contains(r.ToolsUsed, c.Tool) {
			r.ToolsUsed = append(r.ToolsUsed, c.Tool)
			sort.Strings(r.ToolsUsed)
		}
	})
}

// flush writes the current record. Audit failures are logged loudly but
// never stop the investigation; the record is retried at the next phase.
func (a *auditor) flush(ctx context.Context) {
	a.mu.Lock()
	body, err := json.MarshalIndent(a.rec, "", "  ")
	a.mu.Unlock()
	if err == nil {
		err = a.store.put(ctx, a.key, body)
	}
	if err != nil {
		a.logger.Error("audit write failed", "key", a.key, "error", err)
	}
}

func (a *auditor) snapshot() AuditRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	return *a.rec
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
