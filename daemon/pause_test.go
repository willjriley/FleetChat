package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The pause rule is enforced in ROUTING, so every test here posts through the
// board and asserts on PostResult.Woke -- the list built by really enqueueing a
// turn to each recipient -- never on a re-derivation of who should have been
// woken. See thread_test.go for why the stub agents must be able to receive.

func wokeSorted(r PostResult) []string {
	out := append([]string{}, r.Woke...)
	sort.Strings(out)
	return out
}

func TestPausedMemberIsNotWokenByAnyPath(t *testing.T) {
	b := threadCrew() // alice, bob, carol, dave
	b.reg.SetPaused("bob", true)

	// The operator's plain broadcast fans out to the crew -- minus bob.
	r := b.Post("owner", "morning all", nil, nil)
	eq(t, wokeSorted(r), []string{"alice", "carol", "dave"})

	// A deliberate @bob wakes nobody, and the sender is TOLD.
	r = b.Post("owner", "@bob are you there", nil, nil)
	eq(t, wokeSorted(r), nil)
	if !strings.Contains(r.Warning, "bob") || !strings.Contains(strings.ToLower(r.Warning), "paused") {
		t.Errorf("addressing a paused member must warn the sender; got warning %q", r.Warning)
	}

	// A structured `to` naming bob (what the composer chips send) is not resolved.
	r = b.Post("owner", "status?", nil, []string{"bob"})
	eq(t, wokeSorted(r), nil)

	// @all excludes bob.
	r = b.Post("alice", "@all heads up", nil, nil)
	eq(t, wokeSorted(r), []string{"carol", "dave"})
}

func TestPausedMemberInsideAConversationIsNotWokenByReplies(t *testing.T) {
	b := threadCrew()
	// bob joins a conversation while active...
	root := b.Post("owner", "@bob @carol kick-off", nil, nil)
	eq(t, wokeSorted(root), []string{"bob", "carol"})
	// ...then is paused. A reply in that conversation auto-addresses its
	// members; bob must stay recorded on the message but must NOT be woken.
	b.reg.SetPaused("bob", true)
	r := b.PostMsg(PostOpts{Sender: "carol", Text: "update from me", Thread: root.ID})
	eq(t, wokeSorted(r), nil) // carol is the sender; bob is paused; owner is not an agent
	if !hasName(addressed(r.BoardMessage), "bob") {
		t.Errorf("a paused member stays a recorded member of the conversation; To=%v", r.To)
	}
	if !strings.Contains(r.Warning, "bob") {
		t.Errorf("the reply must say bob was not woken; got %q", r.Warning)
	}
	// Unpausing restores the member: the next reply reaches bob again.
	b.reg.SetPaused("bob", false)
	r = b.PostMsg(PostOpts{Sender: "carol", Text: "and again", Thread: root.ID})
	eq(t, wokeSorted(r), []string{"bob"})
	if r.Warning != "" {
		t.Errorf("no warning once unpaused; got %q", r.Warning)
	}
}

func TestPausedMemberIsNotReportedAsUnknown(t *testing.T) {
	// A paused member is still ON the board: addressing it must not be
	// mistaken for a typo ("not on this board"), which would send the operator
	// looking for a spelling error instead of the pause switch.
	b := threadCrew()
	b.reg.SetPaused("dave", true)
	r := b.Post("owner", "@dave ping", nil, nil)
	if strings.Contains(r.Warning, "not on this board") {
		t.Errorf("paused must not read as unknown: %q", r.Warning)
	}
}

func TestActiveExcludesPausedAndAllStillListsThem(t *testing.T) {
	reg := NewRegistry()
	for _, id := range []string{"alice", "bob"} {
		reg.agents[id] = stubAgent(id)
	}
	reg.SetPaused("alice", true)
	if n := len(reg.Active()); n != 1 || reg.Active()[0].id != "bob" {
		t.Errorf("Active must exclude the paused member; got %d", n)
	}
	if n := len(reg.All()); n != 2 {
		t.Errorf("All must still list a paused member (shown as paused, not vanished); got %d", n)
	}
	if !reg.IsPaused("alice") || reg.IsPaused("bob") {
		t.Error("IsPaused must reflect the switch")
	}
}

func TestPausePersistsInTheDurableRoster(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEETCHAT_ROSTER_FILE", filepath.Join(root, "roster.json"))
	if err := writeRoster(root, []RosterEntry{{Name: "bob", Dir: "x"}, {Name: "carol"}}); err != nil {
		t.Fatal(err)
	}
	if !rosterSetPaused(root, "bob", true) {
		t.Fatal("bob has a durable entry; the pause must be recorded")
	}
	got := readRoster(root)
	if len(got) != 2 || got[0].Name != "bob" || !got[0].Paused || got[1].Paused {
		t.Errorf("roster after pause: %+v", got)
	}
	if got[0].Dir != "x" {
		t.Error("pausing must not disturb the rest of the entry")
	}
	if !rosterSetPaused(root, "bob", false) || readRoster(root)[0].Paused {
		t.Error("unpause must be recorded too")
	}
	if rosterSetPaused(root, "nobody", true) {
		t.Error("a name with no durable entry reports false so the caller can say the pause is live-only")
	}
}

func TestPauseEndpointIsOperatorOnlyAndPersists(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEETCHAT_ROSTER_FILE", filepath.Join(root, "roster.json"))
	if err := writeRoster(root, []RosterEntry{{Name: "bob"}}); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	reg.agents["bob"] = stubAgent("bob")
	mux := http.NewServeMux()
	registerPauseRoutes(mux, reg, root)

	call := func(client string, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/control/pause", bytes.NewBufferString(body))
		req.Header.Set("X-Fleet-Client", client)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// An agent (the header value the board tells every agent to send) is refused.
	if rec := call("agent", `{"agent":"bob","paused":true}`); rec.Code != http.StatusForbidden {
		t.Errorf("agent client must be refused; got %d %s", rec.Code, rec.Body.String())
	}
	if reg.IsPaused("bob") {
		t.Error("a refused request must not change state")
	}
	// The operator's page pauses, and the roster records it.
	rec := call("1", `{"agent":"bob","paused":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("operator pause: got %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Paused    bool `json:"paused"`
		Persisted bool `json:"persisted"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.Paused || !resp.Persisted || !reg.IsPaused("bob") || !readRoster(root)[0].Paused {
		t.Errorf("pause must be live AND durable; resp=%+v live=%v roster=%+v", resp, reg.IsPaused("bob"), readRoster(root))
	}
	// Unpause.
	if rec := call("1", `{"agent":"bob","paused":false}`); rec.Code != http.StatusOK || reg.IsPaused("bob") || readRoster(root)[0].Paused {
		t.Errorf("unpause must clear both; got %d", rec.Code)
	}
	// Malformed and unknown.
	if rec := call("1", `{"agent":"bob"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("missing paused flag must be a 400; got %d", rec.Code)
	}
	if rec := call("1", `{"agent":"nobody","paused":true}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown agent must be a 404; got %d", rec.Code)
	}
	if rec := call("1", `{"agent":"../x","paused":true}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad id must be a 400; got %d", rec.Code)
	}
}

func hasName(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
