package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// FleetConfig mirrors fleet.json / fleet.local.json: the DECLARED crew -- who is
// on the team -- as opposed to data/roster.json, which is the durable RUNTIME
// roster (mutated by /control/add and /control/kick). This declared file is the
// "who's on the team" source run.py reads via fleet_file() on every boot; the Go
// daemon previously read only roster.json and so never consulted it at all.
type FleetConfig struct {
	Domain string   `json:"domain"`
	Lead   string   `json:"lead"`
	Crew   []string `json:"crew"`
}

// fleetFile resolves the declared-crew file with the SAME precedence as
// personaBaseDirs and run.py's fleet_file(): the git-ignored fleet.local.json
// (the REAL fleet -- may name real people/infra, so it is never committed) wins
// over the tracked fleet.json (the public demo crew). Returns "" if neither
// exists, which the caller treats as "no declared crew", not an error.
func fleetFile(repoRoot string) string {
	for _, name := range []string{"fleet.local.json", "fleet.json"} {
		p := filepath.Join(repoRoot, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// readFleet loads the resolved fleet file. A missing or corrupt file is never
// fatal -- it returns nil and the caller falls back to an empty crew rather than
// refusing to boot, the same failure posture as readRoster/loadSessions.
func readFleet(repoRoot string) *FleetConfig {
	path := fleetFile(repoRoot)
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var fc FleetConfig
	if json.Unmarshal(b, &fc) != nil {
		log.Printf("[fleet] %s is not valid JSON -- ignoring it (no crew seeded)", filepath.Base(path))
		return nil
	}
	return &fc
}

// seedRosterFromFleet writes data/roster.json from the declared crew when there
// is no durable roster yet, and returns the entries it seeded. This is what
// makes the REAL configured lineup come up on a fresh setup (or after data/ is
// wiped) instead of an empty board: the Go port read roster.json but never
// seeded it from the fleet file, so fleet.local.json was effectively dead on
// this backend and the real agents only loaded if someone had hand-placed the
// roster.
//
// Only crew NAMES are seeded (no working dir -- that is added later per agent),
// each re-validated against validID so a hand-edited fleet file cannot inject a
// bad id, and reserved names ("board"/"all") are skipped. Duplicates collapse.
// If nothing valid remains, no file is written and nil is returned.
func seedRosterFromFleet(repoRoot string) []RosterEntry {
	fc := readFleet(repoRoot)
	if fc == nil || len(fc.Crew) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var entries []RosterEntry
	for _, name := range fc.Crew {
		n := strings.ToLower(strings.TrimSpace(name))
		if n == "" || seen[n] || isReservedName(n) || !validID.MatchString(n) {
			continue
		}
		seen[n] = true
		entries = append(entries, RosterEntry{Name: n})
	}
	if len(entries) == 0 {
		return nil
	}
	// A failed persist is not fatal: the returned entries still bring the crew up
	// THIS boot, and the next boot simply seeds again from the same fleet file.
	if err := writeRoster(repoRoot, entries); err != nil {
		log.Printf("[fleet] could not persist seeded roster: %s", err)
	} else {
		log.Printf("[fleet] seeded data/roster.json with %d agent(s) from the declared crew", len(entries))
	}
	return entries
}
