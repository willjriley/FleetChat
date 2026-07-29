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
// name is display-only), the PASS convention, and the task card API.
// protocolRules() takes no arguments, so it is structurally a compile-time
// constant -- it cannot carry any per-deployment particulars (that is the opsec
// property, verified by construction rather than by naming real identities here).
func TestProtocolRulesComplete(t *testing.T) {
	r := protocolRules()
	// The rules cover what the BOARD governs: how addressing wakes a teammate
	// (@name / @all, bare name = display only), the PASS convention, the task
	// card API, and that the BOARD voices replies (agents must not self-TTS on
	// board turns, which would double up over the board's own speaker).
	for _, must := range []string{"@name", "@all", "display", "PASS", "/threads", "board speaks your replies"} {
		if !strings.Contains(r, must) {
			t.Errorf("protocolRules() is missing a required rule marker: %q", must)
		}
	}
	// A NEGATIVE guard, because this is where the false claim did the most
	// damage: these rules used to tell every agent it was "scoped to your own
	// project folder". Nothing confined it -- --add-dir is additive -- so agents
	// carried a false model of their own containment and would decline work on
	// the strength of it. Asserting that the right words are present cannot catch
	// a failure whose shape is a sentence that should not be there at all.
	//
	// The block is now GONE rather than corrected. The board does not create
	// agents, choose their CLI flags, or decide what they may touch -- the
	// operator does -- so ANY access claim it makes is one it cannot back, and
	// the accurate version of a statement that isn't ours to make is still not
	// ours to make. That is why the flag names are markers too: re-introducing a
	// description of an agent's own permissions is the regression here, whether
	// or not the description happens to be true.
	//
	// (An earlier version of this guard matched only claim phrasings, because it
	// had to coexist with a corrected block that itself said "sandboxed" in a
	// denial. With the block removed there is no such text to accommodate, so
	// the blunt vocabulary match is now the right instrument.)
	for _, never := range []string{
		"scoped to your own", "you're scoped", "you are scoped",
		"confined to your", "sandboxed", "Full permissions",
		"--add-dir", "--dangerously-skip-permissions", "approval-mode",
	} {
		if strings.Contains(r, never) {
			t.Errorf("protocolRules() describes agent access/permissions (%q) -- not the board's to state", never)
		}
	}
	// The envelope's per-message data must NOT have leaked into the static rules.
	if strings.Contains(r, "New message from") {
		t.Error("protocolRules() must not contain the per-turn message envelope")
	}
}
