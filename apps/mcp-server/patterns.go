package main

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// Context engineering: instead of handing the model thousands of raw lines,
// group error lines by a normalised template and return counts, time range,
// and a few request IDs to drill into with get_recent_logs.

type ErrorPattern struct {
	Template   string   `json:"template"`
	Count      int      `json:"count"`
	FirstSeen  string   `json:"first_seen"`
	LastSeen   string   `json:"last_seen"`
	Pods       []string `json:"pods,omitempty"`
	RequestIDs []string `json:"sample_request_ids,omitempty"`
	Sample     string   `json:"sample"`
}

type PatternReport struct {
	LinesScanned int            `json:"lines_scanned"`
	ErrorLines   int            `json:"error_lines"`
	Patterns     []ErrorPattern `json:"patterns,omitempty"`
	Truncated    bool           `json:"truncated,omitempty"`
}

var (
	uuidRE   = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	ipPortRE = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?::\d+)?\b`)
	hexRE    = regexp.MustCompile(`\b[0-9a-f]{12,}\b`)
	numRE    = regexp.MustCompile(`\b\d+(?:\.\d+)?(?:ms|s|µs)?\b`)
)

// errorTemplate masks values that differ per occurrence but keeps words, so the
// group for a wrong DATABASE_HOST still names the host.
func errorTemplate(s string) string {
	s = uuidRE.ReplaceAllString(s, "<uuid>")
	s = ipPortRE.ReplaceAllString(s, "<ip>")
	s = hexRE.ReplaceAllString(s, "<hex>")
	s = numRE.ReplaceAllString(s, "<n>")
	return truncate(s, 400)
}

type parsedLine struct {
	level, message, requestID string
}

// classify understands the platform's JSON logs (slog) and falls back to a
// keyword match for anything else.
func classify(text string) (parsedLine, bool) {
	var j map[string]any
	if strings.HasPrefix(text, "{") && json.Unmarshal([]byte(text), &j) == nil {
		level := strings.ToUpper(str(j["level"]))
		if level != "ERROR" && level != "FATAL" && level != "PANIC" {
			return parsedLine{}, false
		}
		msg := str(j["msg"])
		if e := firstNonEmpty(str(j["error"]), str(j["err"])); e != "" {
			msg += ": " + e
		}
		return parsedLine{level: level, message: msg, requestID: str(j["request_id"])}, true
	}
	lower := strings.ToLower(text)
	for _, kw := range []string{"error", "panic", "fatal", "exception"} {
		if strings.Contains(lower, kw) {
			return parsedLine{level: "ERROR", message: text}, true
		}
	}
	return parsedLine{}, false
}

func errorPatterns(lines []LogLine, top int) PatternReport {
	type group struct {
		ErrorPattern
		pods map[string]bool
	}
	groups := map[string]*group{}
	rep := PatternReport{LinesScanned: len(lines)}
	for _, l := range lines {
		p, ok := classify(l.Text)
		if !ok {
			continue
		}
		rep.ErrorLines++
		key := errorTemplate(p.message)
		g, ok := groups[key]
		if !ok {
			g = &group{ErrorPattern: ErrorPattern{Template: key, FirstSeen: l.Time, Sample: truncate(l.Text, 600)}, pods: map[string]bool{}}
			groups[key] = g
		}
		g.Count++
		g.LastSeen = l.Time
		g.pods[l.Pod] = true
		if p.requestID != "" && len(g.RequestIDs) < 3 {
			g.RequestIDs = append(g.RequestIDs, p.requestID)
		}
	}
	for _, g := range groups {
		for pod := range g.pods {
			g.Pods = append(g.Pods, pod)
		}
		sort.Strings(g.Pods)
		rep.Patterns = append(rep.Patterns, g.ErrorPattern)
	}
	sort.Slice(rep.Patterns, func(i, j int) bool {
		if rep.Patterns[i].Count != rep.Patterns[j].Count {
			return rep.Patterns[i].Count > rep.Patterns[j].Count
		}
		return rep.Patterns[i].Template < rep.Patterns[j].Template
	})
	if len(rep.Patterns) > top {
		rep.Patterns = rep.Patterns[:top]
		rep.Truncated = true
	}
	return rep
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
