package main

import (
	"strings"
	"testing"
)

// TestTeachOnceFlag verifies the teach-once gate: a fresh agent needs teaching,
// MarkTaught flips it off, and MarkTaught is idempotent (so the benign
// double-teach race -- two turns both seeing NeedsTeaching before either marks
// -- can never leave the agent in a bad state). Uses a zero-value Agent because
// NeedsTeaching/MarkTaught touch only a.mu (zero-ready) and a.taught.
func TestTeachOnceFlag(t *testing.T) {
	a := &Agent{}
	if !a.NeedsTeaching() {
		t.Fatal("a fresh agent must need teaching")
	}
	a.MarkTaught()
	if a.NeedsTeaching() {
		t.Fatal("after MarkTaught, an agent must NOT need teaching")
	}
	a.MarkTaught() // idempotent
	if a.NeedsTeaching() {
		t.Fatal("MarkTaught must be idempotent -- still taught")
	}
}

// TestMessageEnvelope pins the per-turn envelope format: real, board-attested
// sender attribution and nothing else (the rules are taught separately, once).
func TestMessageEnvelope(t *testing.T) {
	if got, want := messageEnvelope("will", "hello"), "New message from will: hello"; got != want {
		t.Errorf("messageEnvelope = %q, want %q", got, want)
	}
}

// TestProtocolRulesComplete guards against a split that silently drops a rule:
// the generic ruleset must still teach routing (>>to:), that prose @name is
// display-only, the trust boundary, and the PASS convention. protocolRules()
// takes no arguments, so it is structurally a compile-time constant -- it
// cannot carry any per-deployment particulars (that is the opsec property,
// verified by construction rather than by naming real identities here).
func TestProtocolRulesComplete(t *testing.T) {
	r := protocolRules()
	for _, must := range []string{">>to:", "display", "TRUST BOUNDARY", "PASS"} {
		if !strings.Contains(r, must) {
			t.Errorf("protocolRules() is missing a required rule marker: %q", must)
		}
	}
	// The envelope's per-message data must NOT have leaked into the static rules.
	if strings.Contains(r, "New message from") {
		t.Error("protocolRules() must not contain the per-turn message envelope")
	}
}
