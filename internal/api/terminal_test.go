package api

import (
	"testing"
	"time"
)

func TestTerminalAttemptRateLimitPrunesExpiredAddresses(t *testing.T) {
	terminalAttemptMu.Lock()
	lastTerminalAttempt = make(map[string]time.Time)
	terminalLastPruneTime = time.Time{}
	terminalAttemptMu.Unlock()
	t.Cleanup(func() {
		terminalAttemptMu.Lock()
		lastTerminalAttempt = make(map[string]time.Time)
		terminalLastPruneTime = time.Time{}
		terminalAttemptMu.Unlock()
	})

	now := time.Now()
	if !terminalAttemptAllowed("192.0.2.1", now) {
		t.Fatal("first terminal attempt was rejected")
	}
	if terminalAttemptAllowed("192.0.2.1", now.Add(time.Second)) {
		t.Fatal("rate-limited terminal attempt was accepted")
	}
	if !terminalAttemptAllowed("192.0.2.2", now.Add(terminalRateLimit+time.Second)) {
		t.Fatal("new terminal attempt was rejected")
	}

	terminalAttemptMu.Lock()
	_, firstIPPresent := lastTerminalAttempt["192.0.2.1"]
	terminalAttemptMu.Unlock()
	if firstIPPresent {
		t.Fatal("expired terminal attempt was not pruned")
	}
}
