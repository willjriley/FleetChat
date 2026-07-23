package main

import (
	"testing"
	"time"
)

func typingHas(r *Registry, id string) bool {
	for _, x := range r.TypingNow() {
		if x == id {
			return true
		}
	}
	return false
}

func TestTypingOnThenOff(t *testing.T) {
	r := NewRegistry()
	r.SetTyping("alice", true)
	if !typingHas(r, "alice") {
		t.Fatal("alice should be typing after SetTyping(true)")
	}
	r.SetTyping("alice", false)
	if typingHas(r, "alice") {
		t.Fatal("alice should not be typing after SetTyping(false)")
	}
}

// THE REGRESSION THIS FIX EXISTS FOR: agent.go clears typing only when the
// pendingPrivate queue drains, and that queue desyncs permanently if the CLI
// coalesces two queued turns into one result. The "off" then never arrives and
// the sidebar showed a permanent "…". The TTL must reap it regardless.
func TestTypingStaleEntryIsReapedWithoutAnOff(t *testing.T) {
	r := NewRegistry()
	r.SetTyping("alice", true)
	// Simulate an agent that went quiet without ever emitting the clearing
	// "result" -- backdate its last activity past the TTL.
	r.mu.Lock()
	r.typing["alice"] = time.Now().Add(-typingTTL - time.Second)
	r.mu.Unlock()

	if typingHas(r, "alice") {
		t.Fatal("a stale typing entry must not be reported as typing")
	}
	// and it must be REAPED, not merely filtered, so the map cannot grow.
	r.mu.Lock()
	_, still := r.typing["alice"]
	r.mu.Unlock()
	if still {
		t.Fatal("stale entry should have been deleted by TypingNow")
	}
}

// A long but genuinely active turn must keep its dots: every agent event calls
// TouchTyping, which resets the clock.
func TestTouchTypingKeepsALongTurnAlive(t *testing.T) {
	r := NewRegistry()
	r.SetTyping("alice", true)
	r.mu.Lock()
	r.typing["alice"] = time.Now().Add(-typingTTL + 2*time.Second) // nearly stale
	r.mu.Unlock()

	r.TouchTyping("alice") // an event arrives -> still working
	if !typingHas(r, "alice") {
		t.Fatal("TouchTyping should have kept the entry alive")
	}
}

// TouchTyping must NOT create an entry -- a late stray event after a turn ended
// must never resurrect the indicator.
func TestTouchTypingDoesNotResurrect(t *testing.T) {
	r := NewRegistry()
	r.TouchTyping("bob")
	if typingHas(r, "bob") {
		t.Fatal("TouchTyping must not create a typing entry from nothing")
	}
	r.SetTyping("bob", true)
	r.SetTyping("bob", false)
	r.TouchTyping("bob")
	if typingHas(r, "bob") {
		t.Fatal("TouchTyping must not resurrect a cleared entry")
	}
}

// Reaping one stale agent must not disturb a live one.
func TestTypingReapIsPerAgent(t *testing.T) {
	r := NewRegistry()
	r.SetTyping("alice", true)
	r.SetTyping("carol", true)
	r.mu.Lock()
	r.typing["alice"] = time.Now().Add(-typingTTL - time.Second)
	r.mu.Unlock()

	now := r.TypingNow()
	if len(now) != 1 || now[0] != "carol" {
		t.Fatalf("want only carol typing, got %v", now)
	}
}
