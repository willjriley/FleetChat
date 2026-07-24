package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// RosterEntry mirrors board.py's data/roster.json shape exactly -- the
// persisted "who's on the crew" list run.py itself reads on every startup,
// so an agent added here survives a restart of EITHER backend.
type RosterEntry struct {
	Name string `json:"name"`
	Dir  string `json:"dir,omitempty"`
}

// rosterMu serializes the WHOLE read-modify-write-rename cycle in
// rosterAdd/rosterRemove, both called concurrently from HTTP handlers
// (/control/add, /control/kick). Without it two concurrent calls can each
// read the same base list, and the SECOND write silently clobbers the
// first's addition/removal (last rename wins) -- unlike board.go/threads.go,
// there's no in-memory copy backing this file, so a lost write here is
// unrecoverable, not just a stale read.
var rosterMu sync.Mutex

func rosterPath(repoRoot string) string {
	return filepath.Join(repoRoot, "data", "roster.json")
}

// readRoster distinguishes "no file yet" (nil, silent -- a fresh clone) from
// "file exists but is corrupt" (quarantined + logged, never silently treated
// as an empty crew): a caller building a read-modify-write on a
// wrongly-empty base would otherwise overwrite a real roster with an empty
// one on its very next write.
func readRoster(repoRoot string) []RosterEntry {
	path := rosterPath(repoRoot)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entries []RosterEntry
	if json.Unmarshal(b, &entries) != nil {
		// Timestamped, matching threads.go's quarantine -- a fixed ".bad" would let a
		// SECOND corruption silently overwrite the first quarantined copy.
		bad := path + ".bad-" + itoa(int(time.Now().Unix()))
		if err := os.Rename(path, bad); err == nil {
			log.Printf("[roster] data/roster.json was corrupt -- quarantined to %s", bad)
		}
		return nil
	}
	return entries
}

func writeRoster(repoRoot string, entries []RosterEntry) error {
	if entries == nil {
		entries = []RosterEntry{} // "[]" on disk, never the literal "null"
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, "data"), 0755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	// Atomic: temp file + rename, same reasoning as board.go/threads.go -- a
	// crash mid-write leaves either the whole old file or the whole new one.
	// The temp name is unique per PID (belt-and-suspenders alongside
	// rosterMu, which already serializes every call in THIS process -- this
	// covers a stray second writer that didn't go through these functions).
	path := rosterPath(repoRoot)
	tmp := path + ".tmp." + itoa(os.Getpid())
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// rosterAdd is idempotent -- adding a name already present is a no-op,
// matching /control/add's behavior of returning the existing agent rather
// than erroring.
func rosterAdd(repoRoot, name, dir string) {
	rosterMu.Lock()
	defer rosterMu.Unlock()
	entries := readRoster(repoRoot)
	for _, e := range entries {
		if e.Name == name {
			return
		}
	}
	writeRoster(repoRoot, append(entries, RosterEntry{Name: name, Dir: dir}))
}

// rosterRemove drops a name for good -- the next restart won't bring it
// back, matching board.py's /control/kick.
func rosterRemove(repoRoot, name string) {
	rosterMu.Lock()
	defer rosterMu.Unlock()
	entries := readRoster(repoRoot)
	out := entries[:0]
	for _, e := range entries {
		if e.Name != name {
			out = append(out, e)
		}
	}
	writeRoster(repoRoot, out)
}
