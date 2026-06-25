// Package pipeline ties attribution and classification together into a single
// per-request decision: resolve the app, classify the request, and pick the
// action (reroute or passthrough) — plan §8 "pipeline".
package pipeline

import (
	"net/http"
	"strings"

	"github.com/truefoundry/aitori/internal/appresolve"
	"github.com/truefoundry/aitori/internal/classify"
	"github.com/truefoundry/aitori/internal/config"
)

// Decision is the pipeline's per-request verdict.
type Decision struct {
	AppID    string
	Category string
	Action   string // reroute | passthrough
	Rule     string
	PID      int  // originating process id (0 if unattributed)
	Matched  bool // an explicit rule matched (vs. the heuristic fallback)
}

// Reroute reports whether the decision is to reroute through the gateway.
func (d Decision) Reroute() bool { return d.Action == config.ActionReroute }

// Pipeline computes per-request decisions.
type Pipeline struct {
	resolver    *appresolve.Resolver
	rules       []config.Rule // top-level rules, then per-host actions (precedence order)
	bodyMatters bool          // any rule classifies on the body (match_body)
}

// New constructs a Pipeline from the config: a resolver over the app profiles
// (for tagging) and the effective rule set (top-level rules + per-host actions).
func New(cfg *config.Config) *Pipeline {
	rules := effectiveRules(cfg)
	bodyMatters := false
	for i := range rules {
		if len(rules[i].MatchBody) > 0 {
			bodyMatters = true
			break
		}
	}
	return &Pipeline{
		resolver:    appresolve.New(cfg.Apps),
		rules:       rules,
		bodyMatters: bodyMatters,
	}
}

// BodyMatters reports whether any rule classifies on the request body
// (match_body). When false, a request whose action a host/path/method rule
// already resolved need not have its body buffered for classification.
func (p *Pipeline) BodyMatters() bool { return p.bodyMatters }

// effectiveRules orders policy by precedence: top-level rules first, then rules
// synthesized from intercept_hosts entries. A host entry scoped to specific
// paths/methods only applies to matching requests; for such a host a trailing
// passthrough catch-all is appended automatically, so everything else on that
// host passes through without a separate rule.
func effectiveRules(cfg *config.Config) []config.Rule {
	out := append([]config.Rule{}, cfg.Rules...)
	var scoped []string
	seen := map[string]bool{}
	for _, h := range cfg.InterceptHosts {
		if h.Action != "" || h.Category != "" || h.Scoped() {
			action := h.Action
			// A scoped entry ("trace these endpoints") with no explicit action or
			// category means: govern the matching requests. Without this, the
			// synthesized rule matches but classifies as passthrough (empty
			// category → passthrough), so the listed paths would never reroute.
			if action == "" && h.Category == "" && h.Scoped() {
				action = config.ActionReroute
			}
			out = append(out, config.Rule{
				Name:         "host:" + h.Host,
				Hosts:        []string{h.Host},
				PathPatterns: h.Paths,
				Methods:      h.Methods,
				Action:       action,
				Category:     h.Category,
			})
		}
		if h.Scoped() && !seen[h.Host] {
			seen[h.Host] = true
			scoped = append(scoped, h.Host)
		}
	}
	for _, host := range scoped {
		out = append(out, config.Rule{
			Name:     "host:" + host + ":rest",
			Hosts:    []string{host},
			Category: config.CategoryOther,
			Action:   config.ActionPassthrough,
		})
	}
	return out
}

// Decide resolves the app, classifies the request, and returns the action.
// body is the request body already read (possibly truncated for large bodies).
//
// WebSocket upgrades are forced to passthrough: the gateway does not relay WS by
// default (plan §13). Never reroute a relevant request that is a WS upgrade.
func (p *Pipeline) Decide(req *http.Request, flow appresolve.Flow, body []byte) Decision {
	app := p.resolver.Resolve(flow)
	c := classify.Classify(p.rules, req, body)

	action := c.Action
	rule := c.Rule
	if isWebSocketUpgrade(req) && action == config.ActionReroute {
		action = config.ActionPassthrough
		rule = "websocket-passthrough"
	}

	appID := ""
	if app != nil {
		appID = app.ID
	}
	return Decision{AppID: appID, Category: c.Category, Action: action, Rule: rule, PID: flow.PID, Matched: c.Matched}
}

func isWebSocketUpgrade(req *http.Request) bool {
	if !strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, tok := range strings.Split(req.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
			return true
		}
	}
	return false
}
