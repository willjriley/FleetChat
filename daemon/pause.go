package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
)

// PAUSE: a member that is on the crew but off the board.
//
// Why this exists: the operator sometimes drives an agent by hand in a plain
// CLI session while the board's copy of the same agent is still registered.
// Any board message that reached the board copy then woke a second instance of
// the same identity into the same repo -- two hands on one keyboard. Killing
// the board copy loses its session; kicking it loses its roster entry. Pausing
// keeps both and simply makes the member un-wakeable.
//
// The rule is enforced HERE, in the daemon's routing, not in the page. Hiding
// a name from a sidebar is cosmetic; what matters is that no wake path reaches
// a paused member whatever a client sends:
//
//   - a deliberate @name in a body           -> not merged into the recipients
//   - a structured `to` naming it            -> not resolved to a recipient
//   - @all and the operator's plain broadcast -> the crew it fans out to
//                                               excludes paused members
//   - conversation auto-addressing (threads)  -> a paused member already inside
//                                               a conversation stays recorded
//                                               on the message but is not woken
//
// All four fall out of one fact: routing works off Registry.Active(), never
// Registry.All(). The sender is told when a paused member was addressed
// (PostResult.Warning), because a message that silently reaches fewer people
// than it names is the failure the whole warning mechanism exists to end.
//
// Pause state persists in the durable roster (RosterEntry.Paused) so a daemon
// restart brings the member back paused. A restart that silently un-paused
// someone would be absence read as permission.

// SetPaused marks a member un-wakeable (on) or wakeable again (off). The live
// process is left alone: it keeps its session and simply receives no turns.
func (r *Registry) SetPaused(id string, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused == nil {
		r.paused = map[string]bool{}
	}
	if on {
		r.paused[id] = true
	} else {
		delete(r.paused, id)
	}
}

// IsPaused reports whether a member is currently un-wakeable.
func (r *Registry) IsPaused(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.paused[id]
}

// Active returns the members that CAN be woken: every registered agent that is
// not paused. Routing works off this list. All() still returns everyone, which
// is what the sidebar and the roster API want -- a paused member is shown, as
// paused, rather than vanishing.
func (r *Registry) Active() []*Agent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Agent, 0, len(r.agents))
	for id, a := range r.agents {
		if !r.paused[id] {
			out = append(out, a)
		}
	}
	return out
}

// pausedAddressed reports which of the names a message addresses -- the
// structured `to` (which, after auto-addressing, includes the conversation's
// members) plus any deliberate @name in the body -- are paused. The sender is
// told, rather than left to infer from silence that a member never answered.
// Reuses addressableText and atMentionRe so it sees exactly what routing sees:
// a mention inside a code span or a quoted line is display-only and is not
// reported either.
func pausedAddressed(to []string, text string, isPaused func(string) bool) []string {
	seen := map[string]bool{}
	var out []string
	add := func(n string) {
		n = normName(strings.TrimPrefix(strings.TrimSpace(n), "@"))
		if n == "" || n == "all" || seen[n] || !isPaused(n) {
			return
		}
		seen[n] = true
		out = append(out, n)
	}
	for _, t := range to {
		add(t)
	}
	if text != "" {
		for _, mm := range atMentionRe.FindAllStringSubmatch(addressableText(text), -1) {
			add(mm[1])
		}
	}
	return out
}

// rosterSetPaused records the pause in the durable roster so it survives a
// restart. Reports whether the name had a durable entry: a transiently-spawned
// agent is paused live-only, and the caller says so rather than implying the
// state will persist.
func rosterSetPaused(repoRoot, name string, paused bool) bool {
	rosterMu.Lock()
	defer rosterMu.Unlock()
	entries := readRoster(repoRoot)
	changed := false
	for i := range entries {
		if entries[i].Name == name {
			entries[i].Paused = paused
			changed = true
		}
	}
	if !changed {
		return false
	}
	if err := writeRoster(repoRoot, entries); err != nil {
		log.Printf("[pause] could not persist paused=%v for %q: %s", paused, name, err)
		return false
	}
	return true
}

// registerPauseRoutes mounts POST /control/pause {"agent": id, "paused": bool}.
//
// Operator-only, by the same mechanism the other controls use (the global
// security middleware: POST + X-Fleet-Client + Host/Origin checks), plus one
// refinement: the board tells every agent to send exactly `X-Fleet-Client:
// agent`, and that value is refused here. A member must not be able to silence
// a teammate, or un-silence itself, from inside a turn. This is not a hard
// boundary against a local process that forges the header -- see security.go
// for why loopback controls are not authentication -- but it closes the
// ordinary case, and a session token for the residual is the same follow-up
// the other controls share.
func registerPauseRoutes(mux *http.ServeMux, reg *Registry, repoRoot string) {
	mux.HandleFunc("/control/pause", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			json.NewEncoder(w).Encode(map[string]string{"error": "POST only"})
			return
		}
		if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Fleet-Client")), "agent") {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{"error": "pause is an operator control: agents cannot pause or unpause members"})
			return
		}
		var body struct {
			Agent  string `json:"agent"`
			Paused *bool  `json:"paused"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if !validID.MatchString(body.Agent) {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "bad agent name"})
			return
		}
		if body.Paused == nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "paused (true or false) is required"})
			return
		}
		a, ok := reg.Get(body.Agent)
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "no such agent"})
			return
		}
		reg.SetPaused(body.Agent, *body.Paused)
		persisted := rosterSetPaused(repoRoot, body.Agent, *body.Paused)
		// A pause means "nothing more from this member": a turn already in flight
		// is cut too, with the same soft interrupt the Stop button uses (process
		// and session intact). Only when it IS mid-turn -- an interrupt sent to an
		// idle backend is at best a no-op and at worst not.
		interrupted := false
		if *body.Paused {
			for _, id := range reg.TypingNow() {
				if id == body.Agent {
					if err := a.Interrupt(); err != nil {
						log.Printf("[pause] %q paused but its in-flight turn could not be interrupted: %s", body.Agent, err)
					} else {
						interrupted = true
					}
				}
			}
		}
		log.Printf("[pause] %q paused=%v persisted=%v interrupted=%v", body.Agent, *body.Paused, persisted, interrupted)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok": true, "agent": body.Agent, "paused": *body.Paused,
			"persisted": persisted, "interrupted": interrupted,
		})
	})
}
