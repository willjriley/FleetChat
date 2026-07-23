package main

import (
	"fmt"
	"log"
	"regexp"
	"sync"
	"time"
)

// typingTTL bounds how long a typing entry survives without fresh activity from
// the agent. See SetTyping's comment for why a TTL is required at all.
//
// Sizing it: the CLI runs with --include-partial-messages, so a generating turn
// emits stream events continuously and TouchTyping keeps refreshing this. The
// gap that matters is a turn blocked in a long TOOL call, which can be silent
// for minutes. Because TouchTyping deliberately never re-creates a reaped entry
// (a late event must not resurrect a cleared indicator), reaping too eagerly
// would drop the "…" mid-turn and leave it off for the rest of that turn.
//
// So the two failure modes are asymmetric: too short = a wrong "not typing"
// during a legitimate long tool call, too long = a stuck "…" lingers a bit
// before clearing. Fail toward the less-wrong state: a missing indicator is
// cosmetic, a permanently stuck one is the bug being fixed.
//
// Five minutes makes a mid-turn expiry RARE, not impossible -- an agent that
// spawns subagents or runs a long build can exceed it, and because TouchTyping
// never resurrects, any expiry mid-turn stays wrong for the rest of that turn.
// That residual is accepted here; curing it means fixing the pendingPrivate
// accounting this TTL sits under, not enlarging the constant.
const typingTTL = 5 * time.Minute

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
	// repoRoot locates data/sessions.json. "" disables session persistence
	// entirely (used by tests), which degrades to today's behaviour -- every
	// spawn starts fresh -- rather than erroring.
	repoRoot string
}

func NewRegistry() *Registry {
	return &Registry{agents: make(map[string]*Agent), typing: make(map[string]time.Time)}
}

// NewRegistryWithRoot is the production constructor: repoRoot enables per-agent
// session persistence (data/sessions.json), so a restarted agent resumes its own
// conversation instead of waking with amnesia. NewRegistry stays as-is so the
// existing tests keep their no-persistence behaviour explicitly.
func NewRegistryWithRoot(repoRoot string) *Registry {
	r := NewRegistry()
	r.repoRoot = repoRoot
	return r
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
//
// LOAD-BEARING ASSUMPTION, stated so it is not silently relied on: the net only
// reaps an idle-but-alive agent because such an agent is EVENT-SILENT, and
// route() refreshes on ANY event. If the CLI ever emits idle keepalives, every
// keepalive would refresh the entry and a stuck "…" would outlive the TTL
// indefinitely. True for the desync this fixes; recheck it if the event stream
// gains a heartbeat.
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
		// Refuse an id whose process is still being torn down. Returning the
		// dying agent would hand the caller a corpse; creating a new one would
		// put two live processes on one id. RestartAll doesn't hit this -- its
		// Kill now returns only after confirmed exit -- but a concurrent
		// respawn request could.
		if existing.dying.Load() {
			return nil, fmt.Errorf("agent %q is still shutting down", id)
		}
		return existing, nil // idempotent: spawning an existing id just returns it, never a duplicate
	}
	// Resume this agent's OWN prior conversation if we have one on disk. An
	// explicit ResumeSession from the caller wins, so a deliberate "start this
	// one fresh" is never silently overridden by the stored id.
	attemptedResume := ""
	if r.repoRoot != "" && opts.ResumeSession == "" {
		if sid, ok := loadSessions(r.repoRoot)[id]; ok {
			opts.ResumeSession = sid
			attemptedResume = sid
		}
	}
	a, stdout, err := NewAgent(id, opts)
	if err != nil {
		return nil, err
	}
	a.persona = persona
	// Persist the live session id so the NEXT spawn of this id can resume it.
	// Fires from readLoop's goroutine on system/init, so it must not touch r.mu
	// -- saveSession has its own file lock.
	if r.repoRoot != "" {
		root := r.repoRoot
		a.onSession = func(agentID, sessionID string) { saveSession(root, agentID, sessionID) }
	}
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
		// A resume that never reached system/init did not work -- an expired or
		// server-side-deleted session is the ordinary cause. Drop the stored id
		// so the next spawn starts clean instead of retrying a dead session
		// forever, which would turn one stale id into a permanent respawn loop.
		// Deliberately OUTSIDE r.mu: this does file I/O.
		if attemptedResume != "" && r.repoRoot != "" {
			a.mu.Lock()
			live := a.sessionID
			a.mu.Unlock()
			if live == "" {
				log.Printf("[agent %s] resume of session %s never initialised -- forgetting it; next start will be fresh", id, attemptedResume)
				forgetSession(r.repoRoot, id)
			}
		}
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

// Kill terminates the agent and only THEN releases its id.
//
// It used to delete the map entry first and kill afterwards, which left a
// window where the id looked free while its process was still alive: Spawn
// would happily start a second process for the same agent, and the dying one's
// readLoop was still wired to onMessage, so it could keep posting to the board
// under that id. Now the entry stays until Agent.Kill confirms the process is
// actually gone, and Kill marks the agent dying so a concurrent Spawn refuses
// the id rather than handing back a corpse.
//
// The eviction is guarded by identity (cur == a), matching onExit: readLoop's
// own onExit may have already removed this agent and a replacement may have
// taken the id, and this must not delete that replacement.
func (r *Registry) Kill(id string) error {
	r.mu.Lock()
	a, ok := r.agents[id]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such agent %q", id)
	}

	err := a.Kill() // blocks until the process is confirmed gone (or times out)

	r.mu.Lock()
	if cur, ok := r.agents[id]; ok && cur == a {
		delete(r.agents, id)
	}
	r.mu.Unlock()
	return err
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

// KillAll stops every agent -- shared by the tray's "Quit" and POST /shutdown.
// Errors are LOGGED, not discarded: a failed kill
// means a process that outlived the registry entry -- untracked, unreachable
// through the registry, and still able to produce output. Reporting success
// while that survives is the fail-open case worth being loud about.
func (r *Registry) KillAll() {
	for _, a := range r.All() {
		if err := r.Kill(a.id); err != nil {
			log.Printf("[registry] kill %q failed -- process may have outlived the registry: %s", a.id, err)
		}
	}
}
