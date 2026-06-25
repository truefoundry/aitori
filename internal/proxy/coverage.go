package proxy

import (
	"sync"
	"time"
)

// CoverageStatus reports the observed MITM coverage of a host.
type CoverageStatus struct {
	Host      string    `json:"host"`
	Pinned    bool      `json:"pinned"` // TLS handshake failed (likely cert pinning)
	Failures  int       `json:"failures"`
	LastError time.Time `json:"last_error"`
}

// coverage tracks per-host MITM outcomes so the agent can surface which
// allowlisted hosts could not be decrypted (plan §13: pinning detection).
type coverage struct {
	mu    sync.Mutex
	hosts map[string]*CoverageStatus
}

func newCoverage() *coverage {
	return &coverage{hosts: make(map[string]*CoverageStatus)}
}

func (c *coverage) recordPinning(host string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.hosts[host]
	if s == nil {
		s = &CoverageStatus{Host: host}
		c.hosts[host] = s
	}
	s.Pinned = true
	s.Failures++
	s.LastError = time.Now()
}

func (c *coverage) snapshot() []CoverageStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]CoverageStatus, 0, len(c.hosts))
	for _, s := range c.hosts {
		out = append(out, *s)
	}
	return out
}
