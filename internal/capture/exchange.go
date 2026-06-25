// Package capture defines the internal exchange model used by the optional
// local observability sinks and logs (plan §7). It is deliberately
// secret-free: it never carries tokens, credentials, cookies, or bodies, so it
// is always safe to emit.
package capture

import "time"

// Exchange summarizes one proxied request/response for local debugging. It is
// NOT the governance path (that is the gateway); it exists only for operator
// visibility.
type Exchange struct {
	Time       time.Time `json:"time"`
	App        string    `json:"app,omitempty"`
	Category   string    `json:"category,omitempty"`
	Action     string    `json:"action"` // reroute | passthrough
	Rerouted   bool      `json:"rerouted"`
	Method     string    `json:"method"`
	Scheme     string    `json:"scheme"`
	Host       string    `json:"host"`
	Path       string    `json:"path,omitempty"`
	Query      string    `json:"query,omitempty"`
	Status     int       `json:"status"`
	Bytes      int64     `json:"bytes"`
	DurationMS int64     `json:"duration_ms"`
	// GatewayGap is set when a reroute fell back to the upstream (fail-open).
	GatewayGap bool   `json:"gateway_gap,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Redacted returns a copy with potentially-sensitive fields stripped (query
// string), for sinks configured with redaction on.
func (e Exchange) Redacted() Exchange {
	e.Query = ""
	return e
}
