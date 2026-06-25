// Package classify decides a request's category (llm|mcp|other) and action
// (reroute|passthrough) by matching the owning app's rules, then falling back
// to body heuristics (plan §8.4).
package classify

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/truefoundry/aitori/internal/config"
	"github.com/truefoundry/aitori/internal/hostmatch"
	"github.com/truefoundry/aitori/internal/pathmatch"
)

// Decision is the result of classifying a request.
type Decision struct {
	Category string // llm|mcp|other
	Action   string // reroute|passthrough
	Rule     string // matched rule name, or "heuristic"/"default"
	Matched  bool   // true if an explicit rule matched
}

// DefaultAction returns the default action for a category: llm/mcp reroute,
// everything else passes through.
func DefaultAction(category string) string {
	switch category {
	case config.CategoryLLM, config.CategoryMCP:
		return config.ActionReroute
	default:
		return config.ActionPassthrough
	}
}

// Classify evaluates app's rules against req (with body already read into body,
// possibly truncated) against the given rules (already ordered: top-level rules,
// then per-host actions, then any legacy app-scoped rules). The first matching
// rule wins; if none match, the body heuristic applies.
func Classify(rules []config.Rule, req *http.Request, body []byte) Decision {
	jsonBody := parseJSONObject(body)

	for i := range rules {
		r := &rules[i]
		if ruleMatches(r, req, jsonBody) {
			action := r.Action
			if action == "" {
				action = DefaultAction(r.Category)
			}
			name := r.Name
			if name == "" {
				name = "rule"
			}
			return Decision{Category: r.Category, Action: action, Rule: name, Matched: true}
		}
	}

	cat := heuristicCategory(jsonBody)
	return Decision{Category: cat, Action: DefaultAction(cat), Rule: "heuristic", Matched: false}
}

func ruleMatches(r *config.Rule, req *http.Request, jsonBody map[string]any) bool {
	if len(r.Hosts) > 0 && !hostmatch.MatchAny(r.Hosts, req.Host) {
		return false
	}
	if len(r.PathPrefixes) > 0 && !hasAnyPrefix(req.URL.Path, r.PathPrefixes) {
		return false
	}
	if len(r.PathPatterns) > 0 && !pathmatch.MatchAny(r.PathPatterns, req.URL.Path) {
		return false
	}
	if len(r.Methods) > 0 && !containsFold(r.Methods, req.Method) {
		return false
	}
	if len(r.MatchBody) > 0 && !bodyMatches(r.MatchBody, jsonBody) {
		return false
	}
	return true
}

func hasAnyPrefix(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// bodyMatches implements shallow JSON key presence/value matching. For each
// configured key: the key must be present at the top level; if the configured
// value is non-empty, it must also be equal (loosely, via string form).
func bodyMatches(want map[string]any, got map[string]any) bool {
	if got == nil {
		return false
	}
	for k, wv := range want {
		gv, ok := got[k]
		if !ok {
			return false
		}
		if isEmptyMatcher(wv) {
			continue // presence-only
		}
		if fmt.Sprint(wv) != fmt.Sprint(gv) {
			return false
		}
	}
	return true
}

func isEmptyMatcher(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && s == ""
}

// heuristicCategory implements the fallback heuristics (plan §8.4):
//   - JSON body with "model" + ("messages"|"input"|"prompt") => llm
//   - JSON-RPC 2.0 ("jsonrpc":"2.0" + "method")              => mcp
//   - otherwise                                              => other
func heuristicCategory(b map[string]any) string {
	if b == nil {
		return config.CategoryOther
	}
	if v, ok := b["jsonrpc"]; ok && fmt.Sprint(v) == "2.0" {
		if _, hasMethod := b["method"]; hasMethod {
			return config.CategoryMCP
		}
	}
	if _, hasModel := b["model"]; hasModel {
		_, m := b["messages"]
		_, in := b["input"]
		_, p := b["prompt"]
		if m || in || p {
			return config.CategoryLLM
		}
	}
	return config.CategoryOther
}

func parseJSONObject(body []byte) map[string]any {
	if len(body) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil
	}
	return m
}
