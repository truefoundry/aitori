package proxy

import "testing"

func TestCoverageRecordPinning(t *testing.T) {
	c := newCoverage()
	c.recordPinning("chatgpt.com")
	c.recordPinning("chatgpt.com")
	snap := c.snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(snap))
	}
	if !snap[0].Pinned || snap[0].Failures != 2 || snap[0].Host != "chatgpt.com" {
		t.Errorf("unexpected coverage: %+v", snap[0])
	}
}
