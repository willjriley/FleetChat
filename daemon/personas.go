package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
)

// Faithful port of run_agent.py's persona_base_dirs()/load_agent(): external
// $FLEETCHAT_PERSONAS_DIR, then personas.local/ (git-ignored -- a crew you don't
// want committed), then the committed personas/ (any you choose to add). NOTHING
// ships in either -- a fresh clone has no personas at all, so the board boots
// EMPTY. Same lookup order, same files (agent.json + PERSONA.md).
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

// loadPersona returns (config, personaText, found). A dynamically-added
// agent (no persona folder -- e.g. one just added by pointing at a folder) gets
// a synthesized default persona, exactly like the Python original.
func loadPersona(repoRoot, id string) (PersonaConfig, string) {
	// SECURITY (§6 path-traversal): only a well-formed id may drive a
	// filesystem lookup -- an id like "../../../Users/x/somedir" must never join
	// into a path we read. personaIDRe is the same charset the live registry
	// enforces (validID); a malformed id skips disk entirely and falls through to
	// the synthesized default. This is what makes personaIDRe live rather than
	// dead code, and is applied again at /spawn so a bad id is rejected up front.
	if personaIDRe.MatchString(id) {
		for _, base := range personaBaseDirs(repoRoot) {
			agentJSON := filepath.Join(base, id, "agent.json")
			if b, err := os.ReadFile(agentJSON); err == nil {
				var cfg PersonaConfig
				if json.Unmarshal(b, &cfg) == nil {
					persona := ""
					if pb, err := os.ReadFile(filepath.Join(base, id, "PERSONA.md")); err == nil {
						persona = string(pb)
					}
					if cfg.ID == "" {
						cfg.ID = id
					}
					return cfg, persona
				}
			}
		}
	}
	disp := capitalize(id)
	return PersonaConfig{Name: disp, ID: id, Role: "crew member", Intro: disp + " here, joining the board."},
		"You are " + disp + ", a member of a small agent crew on a team chat board. Be helpful, concise, and collaborative; reply in character."
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
