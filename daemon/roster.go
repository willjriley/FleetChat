package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// RosterEntry mirrors board.py's data/roster.json shape exactly -- the
// persisted "who's on the crew" list run.py itself reads on every startup,
// so an agent added here survives a restart of EITHER backend.
type RosterEntry struct {
	Name string `json:"name"`
	Dir  string `json:"dir,omitempty"`
	// CLI is which backend launches this agent ("claude" default | "gemini" |
	// "qwen"), chosen in the Add/Edit dialog. Persisted here -- the roster is the
	// single source of an agent's run config (dir + cli); there is no per-agent
	// config file. Empty = the daemon's default backend.
	CLI string `json:"cli,omitempty"`
	// FullPerms runs this agent with the approval gate off (claude
	// --dangerously-skip-permissions) -- act on any path / run anything, not just
	// its own folder. Per-agent opt-in from the Add/Edit "full permissions" checkbox.
	FullPerms bool `json:"full_perms,omitempty"`
}

// rosterMu serializes the WHOLE read-modify-write-rename cycle in
// rosterAdd/rosterRemove, both called concurrently from HTTP handlers
// (/control/add, /control/kick). Without it two concurrent calls can each
// read the same base list, and the SECOND write silently clobbers the
// first's addition/removal (last rename wins) -- unlike board.go/threads.go,
// there's no in-memory copy backing this file, so a lost write here is
// unrecoverable, not just a stale read.
var rosterMu sync.Mutex

// rosterPath resolves the ONE durable crew file. It lives OUTSIDE the repo by
// default so that adding an agent can never become a commit risk, and so the
// crew survives a clone, a branch switch, or a wiped data/ directory.
//
// Precedence, most specific first:
//
//	$FLEETCHAT_ROSTER_FILE   -> operator-chosen location
//	<user config dir>/fleetchat/roster.json   -> the default, outside the repo
//
// There is deliberately no in-repo fallback for NEW installs. The old
// data/roster.json is migrated once (see migrateRosterOutOfRepo) and then never
// written again -- two files claiming to be the crew is exactly the split that
// let an added agent look present and not survive a restart.
func rosterPath(repoRoot string) string {
	if env := strings.TrimSpace(os.Getenv("FLEETCHAT_ROSTER_FILE")); env != "" {
		return env
	}
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "fleetchat", "roster.json")
	}
	// Only if the OS gives us no config dir at all.
	return filepath.Join(repoRoot, "data", "roster.json")
}

// migrateRosterOutOfRepo moves a pre-existing in-repo roster to the external
// location, ONCE, on boot. It never overwrites an external roster that already
// exists -- if both are present the external one is authoritative, because that
// is the whole point of having a single source.
//
// The in-repo copy is renamed rather than deleted: if this migration is ever
// wrong, the original is still sitting there to inspect.
func migrateRosterOutOfRepo(repoRoot string) {
	dst := rosterPath(repoRoot)
	if _, err := os.Stat(dst); err == nil {
		return // external roster already exists: it wins, nothing to do
	}
	src := filepath.Join(repoRoot, "data", "roster.json")
	b, err := os.ReadFile(src)
	if err != nil {
		return // nothing to migrate
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		log.Printf("[roster] could not create %s: %s", filepath.Dir(dst), err)
		return
	}
	// ATOMIC, matching writeRoster's own tmp+rename pattern -- and it matters
	// more here, because this runs UNATTENDED AT BOOT. A crash or full disk
	// mid-write would leave a PARTIAL roster at dst, which then WINS on the next
	// boot (Stat(dst) succeeds on a truncated file) while the intact source has
	// already been renamed away and is no longer consulted. Rename is atomic, so
	// dst only ever appears complete.
	tmp := dst + ".tmp." + itoa(os.Getpid())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("[roster] could not stage roster at %s: %s", tmp, err)
		return
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		log.Printf("[roster] could not migrate roster to %s: %s", dst, err)
		return
	}
	_ = os.Rename(src, src+".migrated")
	log.Printf("[roster] migrated crew out of the repo -> %s (old file kept as roster.json.migrated)", dst)
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
	// 0600: the roster records per-agent full_perms grants, so it is a
	// capability record and has no business being world-readable.
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// rosterAdd is idempotent -- adding a name already present is a no-op,
// matching /control/add's behavior of returning the existing agent rather
// than erroring.
func rosterAdd(repoRoot, name, dir, cli string, fullPerms bool) {
	rosterMu.Lock()
	defer rosterMu.Unlock()
	entries := readRoster(repoRoot)
	for _, e := range entries {
		if e.Name == name {
			return
		}
	}
	writeRoster(repoRoot, append(entries, RosterEntry{Name: name, Dir: dir, CLI: cli, FullPerms: fullPerms}))
}

// rosterSetRun updates an existing agent's run config (CLI backend + full-perms)
// in the durable roster, so a change made in the Edit dialog (which respawns the
// live process) also survives the next restart. A name not present is a no-op.
func rosterSetRun(repoRoot, name, cli string, fullPerms bool) {
	rosterMu.Lock()
	defer rosterMu.Unlock()
	entries := readRoster(repoRoot)
	changed := false
	for i := range entries {
		if entries[i].Name == name {
			entries[i].CLI = cli
			entries[i].FullPerms = fullPerms
			changed = true
		}
	}
	if changed {
		writeRoster(repoRoot, entries)
	} else {
		// Not in the durable roster (e.g. a transiently-spawned agent): the run
		// config change applies to the LIVE process but won't survive a restart.
		// Log it so a "my Full-permissions toggle didn't stick" is diagnosable
		// rather than a silent no-op. Fails toward off, the safe direction.
		log.Printf("[roster] %q not in durable roster -- run-config change is live-only, won't persist across restart", name)
	}
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
