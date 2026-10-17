package main

import "regexp"

// Logs can leak credentials. Mask the common shapes before anything is
// returned to the agent (and therefore to a third-party model).
var redactions = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?i)(postgres(?:ql)?://[^:/\s]+:)[^@\s]+@`), "${1}[REDACTED]@"},
	{regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api[_-]?key)(["']?\s*[:=]\s*["']?)[^\s"',}]+`), "${1}${2}[REDACTED]"},
	{regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{10,}`), "Bearer [REDACTED]"},
	{regexp.MustCompile(`\b(AKIA|ASIA)[A-Z0-9]{16}\b`), "[REDACTED_AWS_KEY]"},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`), "[REDACTED_JWT]"},
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`), "[REDACTED_PRIVATE_KEY]"},
}

func redact(s string) string {
	for _, r := range redactions {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}
