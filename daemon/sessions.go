package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// Per-agent Claude session ids, so a restarted agent resumes ITS OWN
// conversation instead of starting blank.
//
// This is the fix for the amnesia the operator kept reporting: "I restart the
// board and you all have amnesia, but I can still see the whole chat." Both
// halves of that were true and they are two different stores. board.jsonl is
// daemon state -- Board.load() replays it on startup, which is why the UI still
// shows the history. An agent's memory is RAM inside its claude child process,
// and KillAll destroys it. The board history was never fed to the agent, and
// NewAgent spawned a virgin process every time, so what the operator could see
// on screen had no connection to what the agent knew.
//
// Deliberately NOT the `-c/--continue` flag, which is the obvious-looking fix
// and is actively harmful here: --continue resumes "the most recent
// conversation in the current directory", and the daemon spawns EVERY agent
// from its own single working directory. All of them would resume the same
// most-recent session and inherit each other's context -- forge waking up as
// shield. That is worse than amnesia; it is identity collapse, and it would
// present as agents mysteriously knowing things they were never told.
// --resume <session-id> with an id stored per agent is the version that
// actually keeps them separate.
var sessionsFileMu sync.Mutex

// Claude session ids are UUIDs. Anchored and length-bounded because this value
// is passed as an ARGV element to the claude binary: a hand-edited or corrupted
// sessions.json must not be able to smuggle a second flag (or anything else)
// into the child's command line. Nothing outside this shape is ever used.
var validSessionID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func sessionsPath(repoRoot string) string {
	return filepath.Join(repoRoot, "data", "sessions.json")
}

// loadSessions restores the agent-id -> session-id map. A missing file is a
// fresh board, not an error. A corrupt file degrades to "no sessions" rather
// than refusing to boot: amnesia is a bad outcome, a daemon that will not start
// is a worse one.
//
// Every entry is re-validated on the way in. The file lives on disk and is
// hand-editable, so it is untrusted input on every read, not just on write.
func loadSessions(repoRoot string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(sessionsPath(repoRoot))
	if err != nil {
		return out
	}
	var m map[string]string
	if json.Unmarshal(b, &m) != nil || m == nil {
		return out
	}
	for id, sid := range m {
		// One bad entry drops itself, never the whole file -- matching how
		// Board.load treats a corrupt JSONL line.
		if validID.MatchString(id) && validSessionID.MatchString(sid) {
			out[id] = sid
		}
	}
	return out
}

// saveSession records one agent's session id, preserving every other entry.
//
// Atomic temp+rename with a PID-unique temp name, matching settings.go and
// threads.go: a crash mid-write leaves either the whole old file or the whole
// new one, never a truncated map that would silently lose every agent's memory
// at once. Best-effort by design -- a failed save costs that agent its history
// on the next restart, which must never be escalated into a failed spawn.
func saveSession(repoRoot, agentID, sessionID string) {
	if !validID.MatchString(agentID) || !validSessionID.MatchString(sessionID) {
		return
	}
	sessionsFileMu.Lock()
	defer sessionsFileMu.Unlock()
	m := loadSessions(repoRoot)
	if m[agentID] == sessionID {
		return // unchanged -- skip the write entirely rather than rewriting the file on every init
	}
	m[agentID] = sessionID
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Join(repoRoot, "data"), 0o755) != nil {
		return
	}
	path := sessionsPath(repoRoot)
	tmp := path + ".tmp." + itoa(os.Getpid())
	if os.WriteFile(tmp, b, 0o644) != nil {
		return
	}
	os.Rename(tmp, path)
}

// forgetSession drops a stored id. Called when a resume ATTEMPT fails, so a
// stale or server-side-expired session can't wedge an agent into a permanent
// crash-respawn loop: the next spawn starts clean instead of retrying an id
// that will never work again.
func forgetSession(repoRoot, agentID string) {
	sessionsFileMu.Lock()
	defer sessionsFileMu.Unlock()
	m := loadSessions(repoRoot)
	if _, ok := m[agentID]; !ok {
		return
	}
	delete(m, agentID)
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Join(repoRoot, "data"), 0o755) != nil {
		return
	}
	path := sessionsPath(repoRoot)
	tmp := path + ".tmp." + itoa(os.Getpid())
	if os.WriteFile(tmp, b, 0o644) != nil {
		return
	}
	os.Rename(tmp, path)
}
