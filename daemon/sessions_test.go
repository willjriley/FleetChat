package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSessions(t *testing.T, root, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "data", "sessions.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const goodSID = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
const goodSID2 = "a1b2c3d4-1111-2222-3333-444455556666"

// The whole point of the feature: an id written by one process is readable by
// the next one. Without this an agent wakes with amnesia while the UI still
// shows it the full history.
func TestSessionSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	saveSession(root, "alice", goodSID)
	saveSession(root, "bob", goodSID2)

	m := loadSessions(root)
	if m["alice"] != goodSID || m["bob"] != goodSID2 {
		t.Fatalf("sessions did not survive: %v", m)
	}
}

// Each agent must keep its OWN session. This is the property the --continue
// flag cannot provide (it resolves to "most recent in this directory", and the
// daemon spawns every agent from one directory), so it is worth asserting
// directly rather than assuming.
func TestSessionsArePerAgentNotShared(t *testing.T) {
	root := t.TempDir()
	saveSession(root, "alice", goodSID)
	saveSession(root, "carol", goodSID2)

	m := loadSessions(root)
	if m["alice"] == m["carol"] {
		t.Fatalf("agents must not share a session id: %v", m)
	}
	// And writing one must not clobber the other.
	if len(m) != 2 {
		t.Fatalf("expected 2 independent entries, got %v", m)
	}
}

// The file is on disk and hand-editable, so it is untrusted on every read. A
// bad entry drops itself; it must never take the valid entries with it.
func TestLoadSessionsRejectsBadEntries(t *testing.T) {
	root := t.TempDir()
	writeSessions(t, root, `{
		"alice":"`+goodSID+`",
		"evil":"--dangerously-skip-permissions",
		"traversal":"../../etc/passwd",
		"BAD NAME":"`+goodSID2+`",
		"notauuid":"12345",
		"bob":"`+goodSID2+`"
	}`)
	m := loadSessions(root)
	if m["alice"] != goodSID || m["bob"] != goodSID2 {
		t.Fatalf("valid entries must survive: %v", m)
	}
	for _, bad := range []string{"evil", "traversal", "BAD NAME", "notauuid"} {
		if _, ok := m[bad]; ok {
			t.Fatalf("entry %q should have been rejected: %v", bad, m)
		}
	}
}

// A session id becomes an argv element on the child's command line. The shape
// check is the thing standing between a hand-edited file and an injected flag,
// so assert the rejection directly rather than trusting the regex by eye.
func TestSessionIDShapeRejectsFlagInjection(t *testing.T) {
	for _, bad := range []string{
		"--dangerously-skip-permissions",
		"3f2504e0-4f89-11d3-9a0c-0305e82c3301 --resume other",
		"",
		"3f2504e0-4f89-11d3-9a0c-0305e82c330",   // one char short
		"3f2504e0-4f89-11d3-9a0c-0305e82c3301x", // trailing junk
	} {
		if validSessionID.MatchString(bad) {
			t.Fatalf("must not accept %q as a session id", bad)
		}
	}
	if !validSessionID.MatchString(goodSID) {
		t.Fatalf("must accept a real uuid")
	}
}

// A corrupt or truncated file degrades to "no sessions" -- amnesia is bad, a
// daemon that refuses to boot is worse.
func TestLoadSessionsToleratesCorruptFile(t *testing.T) {
	root := t.TempDir()
	writeSessions(t, root, `{"alice": "3f2504e0-4f89-11d3-`)
	if m := loadSessions(root); len(m) != 0 {
		t.Fatalf("corrupt file should yield no sessions, got %v", m)
	}
	// A missing file is a fresh board, not an error.
	if m := loadSessions(t.TempDir()); len(m) != 0 {
		t.Fatalf("missing file should yield no sessions, got %v", m)
	}
}

// forgetSession is what stops one stale id from wedging an agent into a
// permanent crash-respawn loop; it must drop only its own entry.
func TestForgetSessionDropsOnlyThatAgent(t *testing.T) {
	root := t.TempDir()
	saveSession(root, "alice", goodSID)
	saveSession(root, "bob", goodSID2)

	forgetSession(root, "alice")

	m := loadSessions(root)
	if _, ok := m["alice"]; ok {
		t.Fatalf("alice should have been forgotten: %v", m)
	}
	if m["bob"] != goodSID2 {
		t.Fatalf("bob must be untouched: %v", m)
	}
	// Forgetting an unknown id is a no-op, not a corruption.
	forgetSession(root, "nobody")
	if len(loadSessions(root)) != 1 {
		t.Fatalf("unexpected change after forgetting an unknown id")
	}
}

// No temp-file litter: a failed or completed save must not leave .tmp.<pid>
// files behind in data/.
func TestSaveSessionLeavesNoTempFiles(t *testing.T) {
	root := t.TempDir()
	saveSession(root, "alice", goodSID)
	entries, err := os.ReadDir(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "sessions.json" {
			t.Fatalf("unexpected leftover file: %s", e.Name())
		}
	}
}
