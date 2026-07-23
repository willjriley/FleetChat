package main

import (
	"fmt"
	"regexp"
	"sync"
	"time"
)

// typingTTL bounds how long a typing entry can survive without fresh activity
// from the agent. See SetTyping's comment for why a TTL is required rather than
// a plain bool.
const typingTTL = 90 * time.Second

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
	typing    map[string]time.Time       // id -> last activity; entries older than typingTTL are stale
}

func NewRegistry() *Registry {
	return &Registry{agents: make(map[string]*Agent), typing: make(map[string]time.Time)}
}

// SetTyping/TouchTyping/TypingNow back GET /typing -- the sidebar's animated "…"
// next to a name mid-turn.
//
// This WAS a plain bool, on the reasoning that board.py needed a TTL map only
// because run_agent.py was a separate process that could die without reporting
// "off", whereas this daemon owns the whole turn lifecycle and fires "off" in
// the same code path as "on". That invariant does NOT hold. agent.go's "off" is
// CONDITIONAL on the pendingPrivate queue having drained (`!stillInFlight`), and
// that queue desyncs permanently when the CLI coalesces two turns queued on the
// same stdin into a SINGLE result: two pushes, one pop, len() never returns to
// zero. The "on" fires, the "off" never does, and an agent idle for half an hour
// shows a permanent "…". (The existing desync warning can't catch this -- it
// only fires in the opposite direction, an event with an EMPTY queue.)
//
// So: back to a TTL, for the same reason board.py had one. The point of a TTL
// here is bounded staleness REGARDLESS of why the "off" was missed -- it is a
// safety net, not a replacement for the explicit clear.
func (r *Registry) SetTyping(id string, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if on {
		r.typing[id] = time.Now()
	} else {
		delete(r.typing, id)
	}
}

// TouchTyping refreshes an entry that is ALREADY typing, so a genuinely long
// turn keeps its dots instead of timing out mid-generation. Deliberately does
// not create an entry: a stray late event after a turn ended must never
// resurrect the indicator.
func (r *Registry) TouchTyping(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.typing[id]; ok {
		r.typing[id] = time.Now()
	}
}

// TypingNow returns the agents currently typing, reaping stale entries as it
// goes so a missed "off" can never pin an agent on indefinitely (and the map
// cannot accumulate dead ids).
func (r *Registry) TypingNow() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.typing))
	cutoff := time.Now().Add(-typingTTL)
	for id, at := range r.typing {
		if at.Before(cutoff) {
			delete(r.typing, id)
			continue
		}
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
	a.onActivity = r.TouchTyping // keeps a long-but-live turn's "…" from timing out
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
