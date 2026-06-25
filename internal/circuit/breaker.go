// Package circuit implements a small three-state circuit breaker used to track
// gateway health. When the gateway is unhealthy the breaker opens, and the
// router fails open (forwards directly to the upstream) instead of repeatedly
// dialing a dead gateway (plan §13).
package circuit

import (
	"sync"
	"time"
)

type state int

const (
	closed state = iota
	open
	halfOpen
)

// Breaker is a thread-safe circuit breaker.
type Breaker struct {
	mu        sync.Mutex
	state     state
	failures  int
	threshold int
	cooldown  time.Duration
	openedAt  time.Time
	probing   bool // a half-open probe is in flight

	now func() time.Time // injectable clock for tests
}

// New returns a breaker that opens after threshold consecutive failures and
// stays open for cooldown before allowing a single probe. A threshold <= 0
// defaults to 5; a cooldown <= 0 defaults to 10s.
func New(threshold int, cooldown time.Duration) *Breaker {
	if threshold <= 0 {
		threshold = 5
	}
	if cooldown <= 0 {
		cooldown = 10 * time.Second
	}
	return &Breaker{threshold: threshold, cooldown: cooldown, now: time.Now}
}

// Allow reports whether a request may be sent to the gateway. In the open
// state it returns false until the cooldown elapses, after which it permits a
// single probe (half-open).
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case closed:
		return true
	case open:
		if b.now().Sub(b.openedAt) >= b.cooldown {
			b.state = halfOpen
			b.probing = true
			return true
		}
		return false
	default: // halfOpen
		if b.probing {
			return false // probe already in flight
		}
		b.probing = true
		return true
	}
}

// Success records a successful gateway interaction, closing the breaker.
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = closed
	b.failures = 0
	b.probing = false
}

// Failure records a failed gateway interaction. Enough consecutive failures
// (or any failure of a half-open probe) opens the breaker.
func (b *Breaker) Failure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == halfOpen {
		b.trip()
		return
	}
	b.failures++
	if b.failures >= b.threshold {
		b.trip()
	}
}

func (b *Breaker) trip() {
	b.state = open
	b.openedAt = b.now()
	b.probing = false
}

// Open reports whether the breaker is currently open (gateway considered down).
func (b *Breaker) Open() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state == open && b.now().Sub(b.openedAt) < b.cooldown
}
