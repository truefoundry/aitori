package circuit

import (
	"testing"
	"time"
)

func TestOpensAfterThreshold(t *testing.T) {
	b := New(3, time.Minute)
	for i := 0; i < 2; i++ {
		b.Failure()
		if !b.Allow() {
			t.Fatalf("breaker opened too early after %d failures", i+1)
		}
	}
	b.Failure() // third
	if b.Allow() {
		t.Fatal("breaker should be open after threshold failures")
	}
	if !b.Open() {
		t.Fatal("Open() should report true")
	}
}

func TestHalfOpenProbeAndRecovery(t *testing.T) {
	now := time.Now()
	b := New(1, 10*time.Second)
	b.now = func() time.Time { return now }

	b.Failure()
	if b.Allow() {
		t.Fatal("expected open")
	}

	// Advance past cooldown: one probe allowed, subsequent denied.
	now = now.Add(11 * time.Second)
	if !b.Allow() {
		t.Fatal("expected probe to be allowed after cooldown")
	}
	if b.Allow() {
		t.Fatal("expected only a single probe in half-open")
	}

	// Successful probe closes the breaker.
	b.Success()
	if !b.Allow() {
		t.Fatal("expected closed after successful probe")
	}
}

func TestHalfOpenProbeFailureReopens(t *testing.T) {
	now := time.Now()
	b := New(1, 10*time.Second)
	b.now = func() time.Time { return now }

	b.Failure()
	now = now.Add(11 * time.Second)
	b.Allow() // probe
	b.Failure()
	if b.Allow() {
		t.Fatal("expected breaker to reopen after failed probe")
	}
}

func TestSuccessResetsFailureCount(t *testing.T) {
	b := New(3, time.Minute)
	b.Failure()
	b.Failure()
	b.Success()
	b.Failure()
	b.Failure()
	if !b.Allow() {
		t.Fatal("success should have reset the failure count")
	}
}
