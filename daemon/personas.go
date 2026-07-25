package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
)

// Per-agent RUN CONFIG (home dir + CLI + roster name/role), resolved from
// external $FLEETCHAT_PERSONAS_DIR, then personas.local/ (git-ignored -- a crew
// you don't want committed), then the committed personas/. NOTHING ships in
// either -- a fresh clone has no crew config, so the board boots EMPTY.
//
// CONFIG ONLY: an agent's IDENTITY is its own home-repo CLAUDE.md, which the CLI
// reads because the process runs from that folder (cmd.Dir). FleetChat injects
// NO persona/system-prompt -- there is deliberately no second identity to drift
// from or contradict the home repo. agent.json carries only where+how it runs.
var personaIDRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

type PersonaConfig struct {
	Name  string `json:"name"`
	ID    string `json:"id"`
	Role  string `json:"role"`
	Intro string `json:"intro"`
	// Dir is the agent's HOME repo -- the folder its process runs from (cwd), so
	// it works as a specialist inside its own project. Read from the same
	// git-ignored personas.local/<id>/agent.json as the rest of the persona, so a
	// real filesystem path never enters the committed repo. Empty = no home
	// folder (the agent runs from the daemon's cwd). A roster entry's own dir
	// (set via the UI folder-picker) takes precedence over this default.
	Dir string `json:"dir,omitempty"`
	// CLI is which backend launches this agent: "claude" (default) | "gemini" |
	// "qwen". Per-agent and from the same git-ignored config, so the fleet can mix
	// CLIs across folders -- Claude in one repo, Gemini in another. See
	// buildCLICommand; only claude is fully wired today.
	CLI string `json:"cli,omitempty"`
}

func personaBaseDirs(repoRoot string) []string {
	dirs := []string{}
	if env := os.Getenv("FLEETCHAT_PERSONAS_DIR"); env != "" {
		dirs = append(dirs, env)
	}
	dirs = append(dirs, filepath.Join(repoRoot, "personas.local"), filepath.Join(repoRoot, "personas"))
	return dirs
}

// loadPersona returns an agent's RUN CONFIG (home dir + CLI + roster name/role)
// from its agent.json. It does NOT load or synthesize any identity/system-prompt:
// the agent's identity is its home-repo CLAUDE.md, read by the CLI from cmd.Dir.
// A dynamically-added agent with no agent.json gets a name/role derived from its id.
func loadPersona(repoRoot, id string) PersonaConfig {
	// SECURITY (§6 path-traversal): only a well-formed id may drive a
	// filesystem lookup -- an id like "../../../Users/x/somedir" must never join
	// into a path we read. personaIDRe is the same charset the live registry
	// enforces (validID); a malformed id skips disk entirely and falls through to
	// the id-derived default. Applied again at /spawn so a bad id is rejected up front.
	if personaIDRe.MatchString(id) {
		for _, base := range personaBaseDirs(repoRoot) {
			agentJSON := filepath.Join(base, id, "agent.json")
			if b, err := os.ReadFile(agentJSON); err == nil {
				var cfg PersonaConfig
				if json.Unmarshal(b, &cfg) == nil {
					if cfg.ID == "" {
						cfg.ID = id
					}
					return cfg
				}
			}
		}
	}
	disp := capitalize(id)
	return PersonaConfig{Name: disp, ID: id, Role: "crew member", Intro: disp + " here, joining the board."}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] -= 32
	}
	return string(r)
}
