package main

import (
	"strings"
	"testing"
)

// TestMessageEnvelope pins the per-turn envelope format: real, board-attested
// sender attribution and nothing else (the rules are delivered separately, at
// launch, as an appended system prompt).
func TestMessageEnvelope(t *testing.T) {
	if got, want := messageEnvelope("will", "hello"), "New message from will: hello"; got != want {
		t.Errorf("messageEnvelope = %q, want %q", got, want)
	}
}

// TestProtocolRulesComplete guards against a split that silently drops a rule:
// the generic ruleset must still teach addressing (@name / @all wake, a bare
// name is display-only), the two access modes, and the PASS convention. protocolRules()
// takes no arguments, so it is structurally a compile-time constant -- it
// cannot carry any per-deployment particulars (that is the opsec property,
// verified by construction rather than by naming real identities here).
func TestProtocolRulesComplete(t *testing.T) {
	r := protocolRules()
	// The rules cover what an agent needs: how addressing wakes a teammate
	// (@name / @all, bare name = display only), the two access modes
	// (folder-locked default -> Full permissions unlock), the task card API,
	// and that the BOARD voices replies (agents must not self-TTS on board
	// turns, which would double up over the board's own speaker).
	for _, must := range []string{"@name", "@all", "display", "PASS", "Full permissions", "/threads", "board speaks your replies"} {
		if !strings.Contains(r, must) {
			t.Errorf("protocolRules() is missing a required rule marker: %q", must)
		}
	}
	// The envelope's per-message data must NOT have leaked into the static rules.
	if strings.Contains(r, "New message from") {
		t.Error("protocolRules() must not contain the per-turn message envelope")
	}
}
