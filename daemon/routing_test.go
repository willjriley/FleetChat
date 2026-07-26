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

// The recipient RESOLVER (routing.go) only ever sees the structured `to` list;
// it never scans prose. Turning a deliberate @name in the body into structured
// recipients is a SEPARATE step (mergeAtMentions), tested at the bottom.

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

// mergeAtMentions (routing.go) is the bridge that lets a deliberate @name in a
// message body wake that teammate: it adds @name / @all matches to the `to` list
// a bare name never matches, so incidental mentions still wake no one.

func TestAtMentionWakesNamedCrew(t *testing.T) {
	eq(t, sortedCopy(mergeAtMentions(nil, "heads up @dave, can you look?", crew)), []string{"dave"})
}

func TestBareNameWakesNobody(t *testing.T) {
	// no @ sigil -> pure display, the cycle-safety guarantee
	eq(t, mergeAtMentions(nil, "I think dave already handled this", crew), nil)
}

func TestAtAllWakesEveryone(t *testing.T) {
	eq(t, sortedCopy(mergeAtMentions(nil, "@all standup in 5", crew)), []string{"all"})
}

func TestAtMentionUnknownIgnored(t *testing.T) {
	eq(t, mergeAtMentions(nil, "ping @nobody-here", crew), nil)
}

func TestAtMentionMergesWithStructuredTo(t *testing.T) {
	// a composer chip (bob) PLUS an @dave in the body -> both, deduped
	eq(t, sortedCopy(mergeAtMentions([]string{"bob"}, "and @dave too", crew)), []string{"bob", "dave"})
}

func TestAtMentionDedupesAgainstTo(t *testing.T) {
	// @carol already addressed via `to` (with the @ prefix) -> no duplicate
	got := mergeAtMentions([]string{"@carol"}, "thanks @carol", crew)
	if len(got) != 1 {
		t.Errorf("expected carol once, got %v", got)
	}
}

func TestAtMentionCaseInsensitive(t *testing.T) {
	eq(t, sortedCopy(mergeAtMentions(nil, "over to you @DAVE", crew)), []string{"dave"})
}

func TestAtMentionIgnoredInFencedCode(t *testing.T) {
	// a pasted diff/log inside a fence must not wake anyone -- the UI wouldn't
	// chip it either, so a wake here would have no visible cause
	body := "here's the log:\n```\n[route] msg from @dave to @all\n```\ndone"
	eq(t, mergeAtMentions(nil, body, crew), nil)
}

func TestAtMentionIgnoredInInlineCode(t *testing.T) {
	eq(t, mergeAtMentions(nil, "the handle `@bob` is just an example", crew), nil)
}

func TestAtMentionIgnoredInBlockquote(t *testing.T) {
	// quoting a teammate's message must not re-summon them
	eq(t, mergeAtMentions(nil, "> @carol said hi\nagreed", crew), nil)
}

func TestAtMentionRealAlongsideQuoted(t *testing.T) {
	// a genuine @erin OUTSIDE code still wakes, even when a fenced block also
	// contains handles (which must be ignored)
	body := "@erin please review:\n```\n@dave @bob touched this\n```"
	eq(t, sortedCopy(mergeAtMentions(nil, body, crew)), []string{"erin"})
}

func TestAtMentionIgnoresEmail(t *testing.T) {
	// "carol" is real crew, but "@carol" glued to a word (no leading boundary) is
	// an address fragment, not an address -- it must NOT wake carol. This is the
	// front-end-parity fix: the composer wouldn't chip it either.
	eq(t, mergeAtMentions(nil, "forward this to bob@carol.com please", crew), nil)
}

func TestAtMentionNeedsLeadingBoundary(t *testing.T) {
	// only a boundary-led @ is deliberate; mid-word "x@dave" is not an address
	eq(t, mergeAtMentions(nil, "seex@dave", crew), nil)
	// start-of-string, space, and "(" count as boundaries
	eq(t, sortedCopy(mergeAtMentions(nil, "@dave", crew)), []string{"dave"})
	eq(t, sortedCopy(mergeAtMentions(nil, "(cc @bob)", crew)), []string{"bob"})
	// a LEADING ">" is a blockquote line, not an address -- it must NOT wake
	// (the MAJOR-1 quote/code exclusion; see TestAtMentionIgnoredInBlockquote).
	// A mid-line ">" before @ still counts as a boundary (UI chip parity).
	eq(t, mergeAtMentions(nil, ">@erin", crew), nil)
	eq(t, sortedCopy(mergeAtMentions(nil, "cc >@erin", crew)), []string{"erin"})
}

func sortedCopy(s []string) []string {
	c := append([]string(nil), s...)
	sort.Strings(c)
	return c
}
