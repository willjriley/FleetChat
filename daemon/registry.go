package main

import (
	"fmt"
	"regexp"
	"sync"
)

var validID = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// Registry is the single source of truth for "which agents exist and are
// alive" -- deliberately ONE map, ONE mutex, ONE owner. Tonight's real
// FleetChat bug (an orphaned duplicate process surviving a restart because
// two independent code paths kept their own disagreeing bookkeeping) is
// exactly the failure mode this is built to make structurally impossible:
// there is nowhere else in this program that spawns or tracks an agent.
type Registry struct {
	mu        sync.Mutex
	agents    map[string]*Agent
	onMessage func(agentID, text string) // wired once, from main.go, to Board.Post
	typing    map[string]bool            // mirrors run_agent.py's board.set_typing: on for the duration of a live turn
}

func NewRegistry() *Registry {
	return &Registry{agents: make(map[string]*Agent), typing: make(map[string]bool)}
}

// SetTyping/TypingNow back GET /typing -- the sidebar's animated "…" next to
// a name mid-turn. Unlike board.py's version (a TTL map, needed because
// run_agent.py is a SEPARATE process that could die without ever reporting
// "off"), this daemon controls the whole turn lifecycle itself: Agent's
// onTyping fires "off" in the same code path as "on", including on error, so
// a plain bool is enough -- there's no separate process that could vanish
// mid-turn and leave a stale entry.
func (r *Registry) SetTyping(id string, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if on {
		r.typing[id] = true
	} else {
		delete(r.typing, id)
	}
}

func (r *Registry) TypingNow() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.typing))
	for id := range r.typing {
		out = append(out, id)
	}
	return out
}

func (r *Registry) Spawn(id string, opts AgentOptions, persona PersonaConfig) (*Agent, error) {
	if !validID.MatchString(id) {
		return nil, fmt.Errorf("bad agent id %q: must match %s", id, validID.String())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.agents[id]; ok {
		return existing, nil // idempotent: spawning an existing id just returns it, never a duplicate
	}
	a, stdout, err := NewAgent(id, opts)
	if err != nil {
		return nil, err
	}
	a.persona = persona
	// If this agent's process ever dies on its own (crash, an unrecoverable
	// auth error, anything NOT routed through Registry.Kill), this is what
	// keeps the registry honest instead of listing a dead agent as alive.
	// Deliberately re-locks rather than reusing the caller's lock: onExit
	// fires later, from readLoop's own goroutine, long after Spawn has
	// already returned and released this lock.
	a.onExit = func() {
		r.mu.Lock()
		if cur, ok := r.agents[id]; ok && cur == a {
			delete(r.agents, id)
		}
		r.mu.Unlock()
	}
	if r.onMessage != nil {
		a.onMessage = r.onMessage
	}
	a.onTyping = r.SetTyping
	r.agents[id] = a
	// Start reading output LAST -- every field readLoop/route() touch is set
	// above. See Agent.Start's doc comment for why this ordering matters.
	a.Start(stdout)
	return a, nil
}

func (r *Registry) Get(id string) (*Agent, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.agents[id]
	return a, ok
}

// Kill removes the agent from the registry and terminates its process
// UNDER THE SAME LOCK as the lookup+delete -- no window where a concurrent
// Spawn(id) could race a Kill(id) and leave two different agents briefly
// both claiming that id. This is the exact property tonight's real bug
// lacked: respawn and restart each had their own separate bookkeeping, so
// they could disagree. Here there's only one map and it's never touched
// outside this file.
func (r *Registry) Kill(id string) error {
	r.mu.Lock()
	a, ok := r.agents[id]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("no such agent %q", id)
	}
	delete(r.agents, id)
	r.mu.Unlock()
	return a.Kill()
}

func (r *Registry) All() []*Agent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Agent, 0, len(r.agents))
	for _, a := range r.agents {
		out = append(out, a)
	}
	return out
}

// RestartAll kills and respawns every currently-registered agent with its
// SAME model/persona/folder. Used by both the tray's "Restart all agents"
// menu action and POST /control/restart -- ONE implementation of "flush the
// whole crew," not two that could quietly disagree the way tonight's real
// orphaned-process bug started (two separate code paths, two separate
// bookkeeping copies).
func (r *Registry) RestartAll() int {
	n := 0
	for _, a := range r.All() {
		id, opts, persona := a.id, a.opts, a.persona
		if err := r.Kill(id); err != nil {
			continue
		}
		if _, err := r.Spawn(id, opts, persona); err == nil {
			n++
		}
	}
	return n
}

// KillAll stops every agent -- shared by the tray's "Quit" and POST
// /shutdown, same reasoning as RestartAll above.
func (r *Registry) KillAll() {
	for _, a := range r.All() {
		_ = r.Kill(a.id)
	}
}
