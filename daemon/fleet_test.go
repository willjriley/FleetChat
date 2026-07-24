package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The git-ignored fleet.local.json (a crew you don't commit) must win over a
// plain fleet.json -- the same precedence personaBaseDirs uses. Placeholder names
// only: this file is committed, so it must never name a real crew.
func TestFleetFilePrefersLocalOverPlain(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLEETCHAT_FLEET_FILE", "") // hermetic: ignore any ambient override
	writeFile(t, filepath.Join(dir, "fleet.json"), `{"crew":["carol"]}`)
	if got := fleetFile(dir); got != filepath.Join(dir, "fleet.json") {
		t.Fatalf("with only fleet.json, want it resolved; got %q", got)
	}
	writeFile(t, filepath.Join(dir, "fleet.local.json"), `{"crew":["alice"]}`)
	if got := fleetFile(dir); got != filepath.Join(dir, "fleet.local.json") {
		t.Fatalf("fleet.local.json must win over fleet.json; got %q", got)
	}
	fc := readFleet(dir)
	if fc == nil || len(fc.Crew) != 1 || fc.Crew[0] != "alice" {
		t.Fatalf("readFleet should load the local crew, got %+v", fc)
	}
}

// An explicit $FLEETCHAT_FLEET_FILE outranks the in-repo overlay, letting an
// operator keep their real fleet fully outside the repo (the documented
// contract + parity with personaBaseDirs). A set-but-missing path falls through.
func TestFleetFileEnvOverrideWins(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fleet.local.json"), `{"crew":["alice"]}`)
	ext := filepath.Join(dir, "external", "myfleet.json")
	writeFile(t, ext, `{"crew":["zoe"]}`)

	t.Setenv("FLEETCHAT_FLEET_FILE", ext)
	if got := fleetFile(dir); got != ext {
		t.Fatalf("FLEETCHAT_FLEET_FILE must win over the overlay; got %q", got)
	}
	if fc := readFleet(dir); fc == nil || len(fc.Crew) != 1 || fc.Crew[0] != "zoe" {
		t.Fatalf("readFleet should load the env-pointed crew, got %+v", fc)
	}

	// A set-but-missing env path must fall through to the overlay, not error.
	t.Setenv("FLEETCHAT_FLEET_FILE", filepath.Join(dir, "does-not-exist.json"))
	if got := fleetFile(dir); got != filepath.Join(dir, "fleet.local.json") {
		t.Fatalf("missing env path should fall through to fleet.local.json; got %q", got)
	}
}

// agentWorkDir is what lands each agent in its own repo (cwd): a real folder is
// used, anything unusable falls back to "" (inherit the daemon cwd) so a bad
// path can never fail the spawn.
func TestAgentWorkDir(t *testing.T) {
	dir := t.TempDir()
	if got := agentWorkDir("x", ""); got != "" {
		t.Fatalf(`empty folder must inherit (""), got %q`, got)
	}
	if got := agentWorkDir("x", dir); got != dir {
		t.Fatalf("an existing dir must be used, got %q", got)
	}
	if got := agentWorkDir("x", filepath.Join(dir, "nope")); got != "" {
		t.Fatalf("a missing dir must fall back to inherit, got %q", got)
	}
	f := filepath.Join(dir, "afile")
	writeFile(t, f, "x")
	if got := agentWorkDir("x", f); got != "" {
		t.Fatalf("a plain file (not a dir) must fall back to inherit, got %q", got)
	}
}

// Seeding writes the real declared crew to data/roster.json when none exists --
// the fix for "the real agents never load" on a fresh board.
func TestSeedRosterFromFleet(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fleet.local.json"),
		`{"domain":"demo","lead":"alice","crew":["alice","bob","carol","dave","erin"]}`)

	got := seedRosterFromFleet(dir)
	if len(got) != 5 {
		t.Fatalf("want 5 seeded agents, got %d: %+v", len(got), got)
	}
	// It must have persisted, so the NEXT boot reads the same crew back.
	persisted := readRoster(dir)
	if len(persisted) != 5 {
		t.Fatalf("seed must persist to data/roster.json, read back %d", len(persisted))
	}
	if persisted[0].Name != "alice" {
		t.Fatalf("crew order should be preserved; got %q first", persisted[0].Name)
	}
}

// A hand-edited fleet file must not be able to smuggle a bad id (path-traversal,
// a reserved name, a flag-shaped token) into the roster, and duplicates collapse.
func TestSeedRosterRejectsBadNames(t *testing.T) {
	dir := t.TempDir()
	crew := []string{"alice", "alice", "BOARD", "all", "../etc", "has space", "ok_1"}
	b, _ := json.Marshal(FleetConfig{Crew: crew})
	writeFile(t, filepath.Join(dir, "fleet.local.json"), string(b))

	got := seedRosterFromFleet(dir)
	// Keep: alice (once), ok_1. Drop: dup alice, "board"/"all" reserved,
	// "../etc" + "has space" invalid id.
	want := map[string]bool{"alice": true, "ok_1": true}
	if len(got) != len(want) {
		t.Fatalf("want %d valid entries, got %d: %+v", len(want), len(got), got)
	}
	for _, e := range got {
		if !want[e.Name] {
			t.Errorf("unexpected name seeded: %q", e.Name)
		}
	}
}

// No fleet file, or a corrupt one, degrades to "no crew" -- never a panic or a
// half-written roster.
func TestSeedRosterNoOrCorruptFile(t *testing.T) {
	dir := t.TempDir()
	if got := seedRosterFromFleet(dir); got != nil {
		t.Fatalf("no fleet file should seed nothing, got %+v", got)
	}
	writeFile(t, filepath.Join(dir, "fleet.local.json"), `{ not valid json`)
	if got := seedRosterFromFleet(dir); got != nil {
		t.Fatalf("corrupt fleet file should seed nothing, got %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "roster.json")); !os.IsNotExist(err) {
		t.Fatalf("corrupt fleet file must not write a roster.json")
	}
}
