package main

import (
	"reflect"
	"sort"
	"testing"
)

var crew = []string{"alice", "bob", "carol", "dave", "erin"}

func woke(sender string, to []string) []string {
	m := resolveRecipients(sender, to, crew)
	out := make([]string, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	sort.Strings(want)
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// The whole point of structured routing: prose is NOT scanned. These test the
// recipient RESOLVER (routing.go), which only ever sees the structured `to`.

func TestHumanUnaddressedBroadcasts(t *testing.T) {
	eq(t, woke("owner", nil), crew) // operator's plain message -> whole crew
}

func TestAgentUnaddressedWakesNobody(t *testing.T) {
	eq(t, woke("carol", nil), nil) // an agent's reply with no directive -> nobody (cycle-proof)
}

func TestExplicitToWakesExactlyThose(t *testing.T) {
	eq(t, woke("owner", []string{"carol"}), []string{"carol"})
	eq(t, woke("owner", []string{"bob", "dave"}), []string{"bob", "dave"})
}

func TestAgentCanAddressAnother(t *testing.T) {
	// the relay case: alice hands off to carol via a structured directive
	eq(t, woke("alice", []string{"carol"}), []string{"carol"})
}

func TestAllWakesEveryoneButSender(t *testing.T) {
	eq(t, woke("alice", []string{"all"}), []string{"bob", "carol", "dave", "erin"}) // not alice itself
	eq(t, woke("owner", []string{"all"}), crew)
}

func TestNeverWakeYourself(t *testing.T) {
	eq(t, woke("carol", []string{"carol", "dave"}), []string{"dave"})
}

func TestUnknownNamesIgnored(t *testing.T) {
	eq(t, woke("owner", []string{"nobody-here", "dave"}), []string{"dave"})
}

func TestBoardSenderNeverWakes(t *testing.T) {
	eq(t, woke("board", nil), nil)             // system announcement, empty
	eq(t, woke("board", []string{"all"}), nil) // even with all
}

// splitDirective (routing.go) is the agent-side half: pull a >>to: directive
// off line 1, strip it, leave prose @mentions untouched in the body.

func TestSplitDirectiveExtractsAndStrips(t *testing.T) {
	to, body := splitDirective(">>to: dave\n@carol did great, passing to dave")
	eq(t, sortedCopy(to), []string{"dave"})
	if body != "@carol did great, passing to dave" {
		t.Errorf("body not stripped: %q", body)
	}
}

func TestSplitDirectiveNoDirective(t *testing.T) {
	to, body := splitDirective("just a normal reply mentioning @bob in passing")
	if to != nil {
		t.Errorf("expected nil recipients, got %v", to)
	}
	if body != "just a normal reply mentioning @bob in passing" {
		t.Errorf("body changed unexpectedly: %q", body)
	}
}

func TestSplitDirectiveMultipleAndAll(t *testing.T) {
	to, _ := splitDirective(">>to: carol, dave, erin\nwork")
	eq(t, sortedCopy(to), []string{"carol", "dave", "erin"})
	to2, _ := splitDirective(">>to: all\nannouncement")
	eq(t, sortedCopy(to2), []string{"all"})
}

func TestSplitDirectiveEmptyMeansNobody(t *testing.T) {
	to, body := splitDirective(">>to:\nquiet note")
	if to == nil || len(to) != 0 {
		t.Errorf("expected empty (non-nil) recipients, got %v", to)
	}
	if body != "quiet note" {
		t.Errorf("body wrong: %q", body)
	}
}

func TestSplitDirectiveBareDirectiveHasEmptyBody(t *testing.T) {
	// A reply that is ONLY a directive (no newline, no content) -> empty body.
	// This is the precondition reg.onMessage relies on to skip posting a blank
	// board bubble (the reviewer's empty-directive finding).
	to, body := splitDirective(">>to: dave")
	eq(t, sortedCopy(to), []string{"dave"})
	if body != "" {
		t.Errorf("bare directive should yield empty body, got %q", body)
	}
}

func TestSplitDirectiveCRLF(t *testing.T) {
	// Windows CRLF: the \r must not end up glued to the recipient name or the body.
	to, body := splitDirective(">>to: dave\r\nbody here")
	eq(t, sortedCopy(to), []string{"dave"})
	if body != "body here" {
		t.Errorf("CRLF body not clean: %q", body)
	}
}

func TestSplitDirectiveOnlyFirstLine(t *testing.T) {
	// A >>to: appearing on a LATER line is NOT a directive -- it's body prose.
	to, body := splitDirective("just chatting\n>>to: carol")
	if to != nil {
		t.Errorf("a mid-message >>to: must not route, got %v", to)
	}
	if body != "just chatting\n>>to: carol" {
		t.Errorf("body should be untouched: %q", body)
	}
}

func sortedCopy(s []string) []string {
	c := append([]string(nil), s...)
	sort.Strings(c)
	return c
}
