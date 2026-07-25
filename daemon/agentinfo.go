package main

// AgentInfo is an agent's RUN CONFIG for the registry and the /roster UI: its
// folder name and where + how it runs (home dir + CLI backend). It carries NO
// role, intro, or system-prompt. An agent's IDENTITY is its own home-repo
// CLAUDE.md, which the CLI reads because the process runs from that folder
// (cmd.Dir) -- FleetChat injects no second identity to drift from it.
//
// Everything here is sourced from data/roster.json (dir + cli set in the
// Add/Edit dialog). There is deliberately no per-agent config file.
type AgentInfo struct {
	Name string
	ID   string
	// Dir is the agent's home repo -- the folder its process runs from (cwd), so
	// it works as a specialist inside its own project. Empty = the daemon's cwd.
	Dir string
	// CLI is which backend launches this agent: "claude" (default) | "gemini" |
	// "qwen". See buildCLICommand; only claude is fully wired today.
	CLI string
}

// agentInfo builds an agent's run info from the durable roster. Name and id are
// the folder name (an agent IS its repo folder); dir and cli come from the
// roster entry the Add/Edit dialog wrote. An id with no roster entry (e.g. one
// spawned directly) falls back to a name derived from the id and no home dir.
func agentInfo(repoRoot, id string) AgentInfo {
	for _, e := range readRoster(repoRoot) {
		if e.Name == id {
			return AgentInfo{Name: e.Name, ID: e.Name, Dir: e.Dir, CLI: e.CLI}
		}
	}
	return AgentInfo{Name: id, ID: id}
}

// capitalize upper-cases the first rune -- used only to render a folder name as
// a display/join label ("forge" -> "Forge").
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
