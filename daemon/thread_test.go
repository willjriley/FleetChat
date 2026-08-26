package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Guards for THREAD AUTO-ADDRESSING (operator request, 2026-08-26).
//
// The fault being fixed: routing lived in the sender's memory. A reply had to
// re-name everyone in the conversation, and an agent that forgot addressed
// NOBODY -- resolveRecipients gives an agent with an empty `to` no recipients
// at all. So the failure was silent, and its worst shape was a correction that
// never arrived while the other party kept building on something false.
//
// Nearly every test here carries a NEGATIVE CONTROL, because "the message
// reached the right people" is a claim that can be true for reasons having
// nothing to do with threading (a broadcast default, an @name in the prose, a
// leftover explicit list). Without a control that must come back EMPTY, a
// passing test proves only that somebody got something.

// stubAgent is an agent that can ACTUALLY RECEIVE a turn: a buffered sendCh and
// a live exited channel, with no subprocess behind them.
//
// This detail decides whether these tests mean anything. With a bare
// &Agent{id:...} the send queue is nil, every SendPrompt fails, and PostResult.Woke
// comes back empty NO MATTER WHAT the routing did -- so an assertion like
// "Woke must be empty" passes for a reason that has nothing to do with the code
// under test. Mutation-testing caught exactly that: deleting the Quiet check
// entirely left the quiet test still green. A buffered channel makes Woke a real
// answer, and the mutation goes red as it should.
func stubAgent(id string) *Agent {
	return &Agent{id: id, sendCh: make(chan sendJob, 8), exited: make(chan struct{})}
}

func threadCrew() *Board {
	reg := NewRegistry()
	for _, id := range []string{"alice", "bob", "carol", "dave"} {
		reg.agents[id] = stubAgent(id)
	}
	return NewBoard(reg, "") // "" = no persistence
}

// addressed reports the recipients STORED on a message, which is what
// auto-addressing produces and what the board shows.
func addressed(m BoardMessage) []string {
	out := append([]string{}, m.To...)
	for i := range out {
		out[i] = normName(out[i])
	}
	sort.Strings(out)
	return out
}

// wakes reports who the board ACTUALLY woke -- PostResult.Woke, the list the
// daemon built by really enqueueing a turn to each recipient. Re-deriving it
// with resolveRecipients here would only re-test the resolver; this tests the
// path a live message takes.
func wakes(r PostResult) []string {
	out := append([]string{}, r.Woke...)
	sort.Strings(out)
	return out
}

func same(t *testing.T, got, want []string, what string) {
	t.Helper()
	sort.Strings(want)
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v, want %v", what, got, want)
		}
	}
}

func has(list []string, name string) bool {
	for _, n := range list {
		if n == name {
			return true
		}
	}
	return false
}

// THE CORE CLAIM: a reply that names nobody still reaches the conversation.
func TestReplyWithNoTagReachesTheThread(t *testing.T) {
	b := threadCrew()
	root := b.PostMsg(PostOpts{Sender: "owner", Text: "kick off", To: []string{"alice", "bob"}})

	// bob replies naming NOBODY -- the exact case that used to reach no one.
	reply := b.PostMsg(PostOpts{Sender: "bob", Text: "on it", Thread: root.Thread})
	same(t, addressed(reply.BoardMessage), []string{"alice", "owner"}, "bob's untagged reply")

	// ...and it genuinely WAKES alice (owner is not an agent, so costs no turn).
	same(t, wakes(reply), []string{"alice"}, "who bob's reply wakes")

	// NEGATIVE CONTROL: the same untagged text OUTSIDE a thread must still
	// reach nobody. Without this, the test above would pass even if the daemon
	// had simply started broadcasting everything.
	loner := b.PostMsg(PostOpts{Sender: "bob", Text: "on it"})
	same(t, addressed(loner.BoardMessage), nil, "untagged reply on a fresh thread")
	same(t, wakes(loner), nil,
		"NEGATIVE CONTROL: a new thread must not auto-address anyone")
}

func TestSenderIsNeverAddressedToThemselves(t *testing.T) {
	b := threadCrew()
	root := b.PostMsg(PostOpts{Sender: "owner", Text: "start", To: []string{"alice"}})
	r1 := b.PostMsg(PostOpts{Sender: "alice", Text: "reply", Thread: root.Thread})
	if has(addressed(r1.BoardMessage), "alice") {
		t.Fatal("alice was auto-addressed to herself -- an agent waking itself is a loop")
	}
	// Second reply in a row: alice must still not appear via her own earlier message.
	r2 := b.PostMsg(PostOpts{Sender: "alice", Text: "more", Thread: root.Thread})
	if has(addressed(r2.BoardMessage), "alice") {
		t.Fatal("alice re-entered her own recipient list on a later message")
	}
}

// Pulling someone new in costs ONE mention; after that they are carried.
func TestAddingSomeoneOnceCarriesThemAfterwards(t *testing.T) {
	b := threadCrew()
	root := b.PostMsg(PostOpts{Sender: "owner", Text: "start", To: []string{"alice"}})
	b.PostMsg(PostOpts{Sender: "alice", Text: "need carol", Thread: root.Thread, To: []string{"carol"}})

	// bob was never here; carol was named once, by someone else, one message ago.
	next := b.PostMsg(PostOpts{Sender: "owner", Text: "and?", Thread: root.Thread})
	same(t, addressed(next.BoardMessage), []string{"alice", "carol"}, "carol carried without re-tagging")

	if has(addressed(next.BoardMessage), "bob") || has(addressed(next.BoardMessage), "dave") {
		t.Fatal("NEGATIVE CONTROL: a non-participant was auto-addressed")
	}
}

// The one that catches an apply-and-forget removal.
func TestDropIsDurableNotJustForOneMessage(t *testing.T) {
	b := threadCrew()
	root := b.PostMsg(PostOpts{Sender: "owner", Text: "start", To: []string{"alice", "carol"}})

	// Remove carol while replying -- she must not be woken by THIS message.
	out := b.PostMsg(PostOpts{Sender: "owner", Text: "carol not needed", Thread: root.Thread, Drop: []string{"carol"}})
	if has(addressed(out.BoardMessage), "carol") {
		t.Fatal("dropped in the same message but still addressed by it")
	}

	// ...and must STAY out on later messages that say nothing about her. If the
	// drop were applied and forgotten, this is where she reappears.
	later := b.PostMsg(PostOpts{Sender: "alice", Text: "carrying on", Thread: root.Thread})
	same(t, addressed(later.BoardMessage), []string{"owner"}, "after a durable drop")

	// Re-adding is just naming her again -- removal must not be permanent.
	back := b.PostMsg(PostOpts{Sender: "owner", Text: "carol back", Thread: root.Thread, To: []string{"carol"}})
	if !has(addressed(back.BoardMessage), "carol") {
		t.Fatalf("re-adding a dropped member failed: %v", addressed(back.BoardMessage))
	}
	// And she is carried again from then on.
	after := b.PostMsg(PostOpts{Sender: "alice", Text: "ok", Thread: root.Thread})
	same(t, addressed(after.BoardMessage), []string{"carol", "owner"}, "re-added member is carried")
}

// Auto-addressing turns every "thanks" into N turns unless this works.
func TestQuietJoinsTheThreadButWakesNobody(t *testing.T) {
	b := threadCrew()
	root := b.PostMsg(PostOpts{Sender: "owner", Text: "start", To: []string{"alice", "bob"}})

	ack := b.PostMsg(PostOpts{Sender: "alice", Text: "thanks", Thread: root.Thread, Quiet: true})
	if len(ack.Woke) != 0 {
		t.Fatalf("a quiet ack woke %v -- it must cost no turns", ack.Woke)
	}
	// It is still a real, visible message with its recipients recorded...
	same(t, addressed(ack.BoardMessage), []string{"bob", "owner"}, "quiet still records who it was for")
	// ...and quiet must not remove anyone from the conversation.
	next := b.PostMsg(PostOpts{Sender: "owner", Text: "next", Thread: root.Thread})
	same(t, addressed(next.BoardMessage), []string{"alice", "bob"}, "membership survives a quiet post")
}

// "board" is the system announcer; auto-addressing it would be a message to
// something that cannot read, and it would show up as a member forever.
func TestSystemSenderNeverJoinsAThread(t *testing.T) {
	b := threadCrew()
	root := b.PostMsg(PostOpts{Sender: "owner", Text: "start", To: []string{"alice"}})
	b.PostMsg(PostOpts{Sender: "board", Text: "restarting", Thread: root.Thread})
	next := b.PostMsg(PostOpts{Sender: "alice", Text: "still here", Thread: root.Thread})
	if has(addressed(next.BoardMessage), "board") {
		t.Fatal("the system sender became a thread member")
	}
}

// Membership is DERIVED from the log. That claim is only true if it survives a
// restart, so prove it against a real file rather than trusting the design.
func TestMembershipIsRebuiltFromTheLogOnRestart(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "board.jsonl")

	newReg := func() *Registry {
		r := NewRegistry()
		for _, id := range []string{"alice", "bob", "carol"} {
			r.agents[id] = stubAgent(id)
		}
		return r
	}

	b1 := NewBoard(newReg(), file)
	root := b1.PostMsg(PostOpts{Sender: "owner", Text: "start", To: []string{"alice", "carol"}})
	b1.PostMsg(PostOpts{Sender: "alice", Text: "drop carol", Thread: root.Thread, Drop: []string{"carol"}})

	if _, err := os.Stat(file); err != nil {
		t.Fatalf("board file was never written: %v", err)
	}

	// Fresh Board over the SAME file -- as after a daemon restart.
	b2 := NewBoard(newReg(), file)
	after := b2.PostMsg(PostOpts{Sender: "bob", Text: "joining in", Thread: root.Thread})

	// alice and owner carried across the restart; carol's drop survived too.
	same(t, addressed(after.BoardMessage), []string{"alice", "owner"}, "membership after restart")
}

// An explicit ">>to:" has always meant "deliberately nobody". Auto-addressing
// must not override an explicit intent -- that is the mapping main.go makes.
func TestExplicitEmptyDirectiveMeansNobodyNotAutoAddress(t *testing.T) {
	to, body := splitDirective(">>to:\nack, nothing needed")
	if to == nil {
		t.Fatal("an explicit empty directive must be non-nil (explicit nobody), not nil (no directive)")
	}
	if len(to) != 0 {
		t.Fatalf("expected an empty recipient list, got %v", to)
	}
	if body == "" {
		t.Fatal("the body must survive the directive being stripped")
	}
	// And the no-directive case must stay distinguishable, since that is the
	// one auto-addressing exists to serve.
	to2, _ := splitDirective("just replying")
	if to2 != nil {
		t.Fatalf("a reply with no directive must yield nil, got %v", to2)
	}
}

// The rules agents are launched with must DESCRIBE the routing they actually
// get. This is not pedantry: before threading, the rules told agents "an @name
// is the ONLY thing that wakes someone" and "a bare acknowledgment needs no @".
// Both are now false -- a reply reaches the conversation, and a bare
// acknowledgment wakes everyone in it unless it is quiet. An agent operating on
// stale rules would either re-tag needlessly or spend everyone a turn on
// "thanks", and nothing in the system would flag it.
func TestProtocolRulesDescribeThreadingNotTheOldModel(t *testing.T) {
	r := protocolRules()

	for _, stale := range []string{
		"ONLY thing that wakes",
		"bare acknowledgment or",
	} {
		if strings.Contains(r, stale) {
			t.Errorf("rules still carry the pre-threading claim %q -- agents would act on a false model", stale)
		}
	}
	for _, needed := range []string{
		"AUTOMATICALLY", // replies stay in the conversation
		">>to:",         // the acknowledge-without-waking path
		"@name",         // bringing someone new in
		"PASS",          // unchanged behaviour that must survive the rewrite
	} {
		if !strings.Contains(r, needed) {
			t.Errorf("rules never mention %q -- agents cannot use what they are not told about", needed)
		}
	}
}

// normName must be IDENTITY over every id an agent can actually have.
//
// Routing and membership compare on normName, while the registry keys on the raw
// id. That is only safe while the two can never disagree -- otherwise two
// distinct registry entries would be one person to the router, and a message
// meant for one would wake the other.
//
// Today they cannot disagree, because validID is ^[a-z0-9_-]{1,32}$: already
// lowercase, no "@", no spaces, so normName changes nothing. This test exists
// because that guarantee is a PROPERTY OF validID, not of normName -- widening
// validID to accept uppercase (or "@") would make the collision real, silently,
// with no other test failing. This is the one that would go red.
func TestNormNameIsIdentityOverEveryLegalAgentID(t *testing.T) {
	for _, id := range []string{"a", "hope", "dm-x", "agent_1", "x9", "a-b_c-9",
		strings.Repeat("z", 32)} {
		if !validID.MatchString(id) {
			t.Fatalf("test fixture %q is not a legal id -- fix the fixture", id)
		}
		if got := normName(id); got != id {
			t.Errorf("normName(%q) = %q: routing would not match the registry key", id, got)
		}
	}

	// The invariant depends on validID rejecting anything normName would alter.
	// If any of these ever become legal, the identity above breaks and routing
	// can conflate two distinct agents.
	for _, bad := range []string{"Hope", "HOPE", "hOpe", "@hope", " hope", "hope "} {
		if validID.MatchString(bad) {
			t.Errorf("validID now accepts %q, which normName rewrites to %q -- "+
				"two registry entries would collide into one routing identity; "+
				"add a normName collision check at registration", bad, normName(bad))
		}
	}
}
