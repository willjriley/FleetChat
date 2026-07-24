package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// The real FleetChat daemon: same repo, same server/web/ frontend
// (unmodified), same fleet.local.json/personas.local/ as the Python board --
// this IS "our fleet," running on the new plumbing, not a separate thing.
// Runs on the SAME port the Python board used (8137) -- this REPLACES it in
// place, not a parallel copy on a different port. The tray icon IS the
// service presence; there is no separate supervisor process.
const daemonPort = "8137"

// kokoroVoices: the fixed high-quality (Kokoro) voice ids the per-agent voice
// picker offers. English set -- the useful ones. Speech is server-only now (the
// browser Web-Speech path was removed), so these are THE voices.
var kokoroVoices = []string{
	"af_heart", "af_bella", "af_sky", "af_sarah", "af_nicole",
	"am_adam", "am_michael", "am_echo", "am_onyx", "am_fenrir", "am_liam", "am_puck",
	"bf_alice", "bf_emma", "bf_isabella", "bf_lily",
	"bm_george", "bm_daniel", "bm_lewis",
}

func main() {
	// Resolve the repo root and install the log tee FIRST -- BEFORE
	// killOtherInstances -- so the single-instance / restart-handoff logs (the very
	// diagnostics this tee exists to surface under `start /min`) land in
	// data/daemon.log too, not just stderr. Resolving repoRoot has no side effects
	// and doesn't depend on the single-instance kill.
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		log.Fatalf("can't resolve repo root: %s", err)
	}
	// Tee the daemon log to data/daemon.log (in addition to stderr) so it's ALWAYS
	// inspectable no matter how the daemon was launched: fleet-up.bat's `start /min`
	// sends stderr to a hidden console you can't read, so without this a live
	// `Get-Content data\daemon.log -Wait` tail would only work for a redirected
	// launch. logFile is FIRST in the MultiWriter so the durable file sink still
	// gets each line even if stderr errors (a torn-down hidden console): MultiWriter
	// returns on the FIRST writer's error. Append keeps history across restarts;
	// best-effort -- fall back to stderr-only rather than failing to boot.
	if logFile, ferr := os.OpenFile(filepath.Join(repoRoot, "data", "daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); ferr == nil {
		log.SetOutput(io.MultiWriter(logFile, os.Stderr))
	} else {
		log.Printf("[daemon] could not open data/daemon.log for tee-logging (%s) -- stderr only", ferr)
	}

	// SINGLE-BOARD RULE, enforced by construction: a fresh start supersedes any
	// prior instance rather than coexisting with it. Kill other copies of this
	// daemon (and their agent trees) BEFORE standing up a board, so there is only
	// ever one board, on one port. See killOtherInstances (per-OS file).
	killOtherInstances()

	webDir := filepath.Join(repoRoot, "server", "web")

	// WithRoot: enables data/sessions.json, so agents resume their own
	// conversations across a board restart instead of waking blank while the
	// UI still shows the full history back to them.
	reg := NewRegistryWithRoot(repoRoot)
	board := NewBoard(reg, filepath.Join(repoRoot, "data", "board.jsonl"))
	reg.onMessage = func(agentID, text string) {
		// An agent's reply carries its routing as a structured >>to: directive
		// on the first line (splitDirective strips it). No directive -> nil
		// recipients -> resolveRecipients gives an agent sender NOBODY, so a
		// plain reply can never wake a teammate (cycle-proof). Prose @name in
		// the body is display-only.
		to, body := splitDirective(text)
		if strings.TrimSpace(body) == "" {
			// A reply that is ONLY a directive (or empty) has nothing to say --
			// don't post a blank board bubble (and a bare directive with no
			// content is a no-op wake anyway).
			return
		}
		board.Post(agentID, body, nil, to)
	}
	threads := NewThreadStore(filepath.Join(repoRoot, "data", "threads.json"))

	// Routing debug log: default ON (FLEETCHAT_DEBUG=0 forces it off at start).
	// We're actively chasing a wake-cycle, so start with the trace on; it's
	// one line per message and toggleable live via /control/debug or /debug.
	routeDebug.Store(os.Getenv("FLEETCHAT_DEBUG") != "0")

	// The board (HTTP server + crew) is a start/stoppable unit (see lifecycle.go),
	// assigned + started further down. Declared here so the /control/board handler
	// can close over it. bootstrapFleet is no longer called inline -- it's the
	// board's onStart, so it runs on every Start (initial boot AND "Start board").
	var bs *boardServer

	// Lightweight in-memory settings: voice assignments and model overrides.
	// Those two still have no real backend behind them (no --model wiring on
	// respawn), so they stay in-memory -- good enough for the Settings modal
	// and slash commands within a session, not a false promise of surviving a
	// restart.
	//
	// ttsMuted/voiceMode are the EXCEPTION and are now persisted (settings.go).
	// The original "no server-side TTS speaker" justification for keeping them
	// in-memory stopped being true once the Kokoro sidecar landed: losing
	// voiceMode on restart silently reverted it to "auto", which let the
	// browser speech fallback talk OVER the server-side speaker.
	var settingsMu sync.Mutex
	ttsMuted, voiceMode := loadVoicePrefs(repoRoot, false, "auto")
	voiceAssign := map[string]string{}
	modelOverride := map[string]string{}
	var speakerSeen time.Time       // last /control/tts heartbeat from a real server-side speaker (e.g. fleet-speaker.bat), see speaker_active()'s 30s TTL in board.py
	vm := newVoiceManager(repoRoot) // orchestrates the OPTIONAL Python HQ-voice sidecar (download + speaker) -- see voices.go

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(webDir))) // the REAL index.html/tasks.html, unmodified, served from their real location

	mux.HandleFunc("/tasks", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(webDir, "tasks.html"))
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "version": "FleetChat-daemon/0.1", "control": true})
	})

	mux.HandleFunc("/roster", func(w http.ResponseWriter, r *http.Request) {
		type rosterEntry struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Role string `json:"role"`
			CLI  string `json:"cli"`
			Dir  string `json:"dir"` // the agent's home folder (its cwd), for the Edit dialog to show
		}
		out := make([]rosterEntry, 0)
		for _, a := range reg.All() {
			cli := a.opts.CLI
			if cli == "" {
				cli = "claude" // the default backend when a persona doesn't set one
			}
			out = append(out, rosterEntry{ID: a.id, Name: a.persona.Name, Role: a.persona.Role, CLI: cli, Dir: a.opts.Folder})
		}
		// reg.All() walks a Go map -- deliberately randomized iteration order by
		// language design -- so without this sort the sidebar reshuffles on every
		// 4s /roster poll instead of holding a stable order.
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"roster": out})
	})

	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		since := 0
		if s := r.URL.Query().Get("since"); s != "" {
			fmt.Sscanf(s, "%d", &since)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"messages": board.Since(since)})
	})

	mux.HandleFunc("/post", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		// Read the raw bytes first (rather than decoding straight off r.Body) so a
		// rejected request is actually diagnosable -- BOTH server-side (logged, see
		// below) and, more importantly, to the SENDER itself: a bare "bad request"
		// with no detail tells a caller nothing about what to fix. This surfaced
		// chasing an agent's transport-drop report: a malformed/truncated body (e.g. a
		// client miscounting byte-length vs. character-length for multi-byte UTF-8
		// like emoji, which run 4 bytes each) is correctly REJECTED rather than
		// silently stored as garbage -- but the sender deserves to know exactly
		// which of these three distinct problems it hit, not one generic error for
		// all of them.
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 64*1024))
		reason := ""
		var body struct {
			Sender string   `json:"sender"`
			Text   string   `json:"text"`
			Tags   []string `json:"tags"`
			// To is the STRUCTURED recipient list -- who to notify. The frontend
			// sends this (from the composer's @-chips); routing uses it, never a
			// prose scan. Absent -> sender-type default (human broadcast, agent
			// nobody). An @name in Text is display-only.
			To []string `json:"to"`
		}
		switch {
		case !utf8.Valid(raw):
			reason = "request body is not valid UTF-8 -- check how the text was encoded before sending"
		case json.Unmarshal(raw, &body) != nil:
			reason = "malformed JSON -- often a truncated body from a Content-Length that undercounted multi-byte characters (each emoji is 4 bytes in UTF-8, not 1)"
		case body.Sender == "" || body.Text == "":
			reason = "sender and text are both required and must be non-empty"
		case !isPrintableASCII(body.Sender):
			// The sender is the ATTESTED identity messageEnvelope shows agents, so it
			// must not carry characters that hide or fake an identity. Requiring
			// printable ASCII kills the whole impersonation class in one rule:
			// zero-width/control/format runes (ZWSP U+200B, BOM U+FEFF, NUL) are
			// non-ASCII or control, and Latin-lookalike homoglyphs (Cyrillic
			// U+0455 'ѕ') are non-ASCII -- both would otherwise normalize/render
			// as a real agent. Agent ids are already [a-z0-9_-] and operator
			// names are ASCII in practice, so nothing legitimate is excluded.
			reason = "sender must be printable ASCII only -- no control, zero-width, or non-Latin lookalike characters (they can impersonate another identity)"
		}
		if reason != "" {
			log.Printf("[post] rejected (%s): %d byte(s), body=%s", reason, len(raw), truncate(string(raw), 200))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": reason})
			return
		}
		// Identity attestation (fixes the "identity is self-asserted" defect):
		// a real agent NEVER posts over HTTP -- it speaks through its own
		// process, whose stdout the daemon reads in reg.onMessage, so that
		// identity is attested by the daemon, not claimed in-band. Therefore an
		// HTTP /post that claims to BE an agent (or the reserved "board" system
		// sender) is, by construction, someone else asserting that name -- an
		// agent following a card's instructions, a stray script, a malicious
		// page on loopback. Refuse it. isReservedOrKnownAgent normalizes case/
		// whitespace and checks the reserved name + the LIVE registry + the
		// DURABLE roster (so a spoof can't slip through the window while an
		// agent is dead/restarting and momentarily absent from the live map).
		if isReservedOrKnownAgent(body.Sender, reg, repoRoot) {
			log.Printf("[post] REFUSED impersonation: HTTP post claimed protected sender %q", body.Sender)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{"error": "sender '" + body.Sender + "' is a reserved/agent identity; agents speak through their own process, not HTTP -- post under a human/operator name"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(board.Post(body.Sender, body.Text, body.Tags, body.To))
	})

	// GET /typing: mirrors board.py's real shape -- {typing:[ids...],
	// speaking:[ids...]}. "speaking" stays permanently empty: that field
	// exists for a server-side TTS speaker announcing whose reply it's
	// voicing right now, and this daemon has no such speaker (the operator's kokoro
	// TTS is a personal Claude Code tool, not a fleet-board speaker
	// service). An always-empty array is the honest answer, not a stub.
	mux.HandleFunc("/typing", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"typing": reg.TypingNow(), "speaking": []string{}})
	})

	mux.HandleFunc("/threads", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]interface{}{"threads": threads.List()})
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var data map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		op, _ := data["op"].(string)
		thread, errMsg, code := threads.Op(op, data)
		w.WriteHeader(code)
		if errMsg != "" {
			json.NewEncoder(w).Encode(map[string]string{"error": errMsg})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "thread": thread})
	})

	// /control/pick + /control/add: the real "+ Add agent" flow -- native OS
	// folder dialog, then spawn an agent named after that folder, exactly
	// matching board.py's own behavior (just PowerShell instead of tkinter).
	mux.HandleFunc("/control/pick", func(w http.ResponseWriter, r *http.Request) {
		folder, err := nativeFolderPicker()
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": "native picker unavailable"})
			return
		}
		if folder == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "cancelled"})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "folder": folder})
	})

	// /control/browse: a server-side directory lister so the UI can offer a real
	// folder picker that WORKS HEADLESS. The browser's own folder input can't hand
	// back an absolute path (security), and the native OS dialog needs a desktop --
	// but the daemon has filesystem access, so it lists sub-folders + their real
	// absolute paths and the browser navigates them. Read-only: folder NAMES only,
	// never file contents. GET, Host-checked by the middleware like other reads.
	mux.HandleFunc("/control/browse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		dirs, cur, parent, err := listDirs(r.URL.Query().Get("path"))
		if err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"path": cur, "parent": parent, "dirs": dirs})
	})

	mux.HandleFunc("/control/add", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Folder string `json:"folder"`
			CLI    string `json:"cli"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if body.Folder == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "a folder is required"})
			return
		}
		info, err := os.Stat(body.Folder)
		if err != nil || !info.IsDir() {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "that folder does not exist"})
			return
		}
		name := sanitizeAgentName(filepath.Base(body.Folder))
		if name == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "cannot derive an agent name from that folder"})
			return
		}
		if isReservedName(name) {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "'" + name + "' is a reserved name (system/broadcast identity) -- pick a different folder"})
			return
		}
		persona, personaText := loadPersona(repoRoot, name)
		cli := persona.CLI
		if body.CLI != "" {
			cli = body.CLI // the operator's explicit pick in the Add-agent dialog wins over the persona default
		}
		a, err := reg.Spawn(name, AgentOptions{Folder: body.Folder, Persona: personaText, CLI: cli}, persona)
		if err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		rosterAdd(repoRoot, a.id, body.Folder)
		announceJoin(board, a.id, persona)
		log.Printf("[daemon] added agent %q from folder %s", a.id, body.Folder)
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "added": a.id})
	})

	mux.HandleFunc("/spawn", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Persona string `json:"persona"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if isReservedName(body.ID) {
			http.Error(w, "'"+body.ID+"' is a reserved name (system/broadcast identity)", http.StatusBadRequest)
			return
		}
		if !personaIDRe.MatchString(body.ID) {
			// SECURITY (§6 path-traversal): reject a malformed id up front so it can't drive
			// the persona-file lookup into a path traversal; loadPersona guards again.
			http.Error(w, "id must be lowercase letters, digits, '-' or '_' (no path separators)", http.StatusBadRequest)
			return
		}
		persona, personaText := loadPersona(repoRoot, body.ID)
		if body.Persona != "" {
			personaText = body.Persona // explicit override wins over the on-disk persona file
		}
		a, err := reg.Spawn(body.ID, AgentOptions{Model: body.Model, Persona: personaText}, persona)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"ok": "true", "id": a.id})
	})

	mux.HandleFunc("/kill", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID string `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if err := reg.Kill(body.ID); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"ok": "true", "id": body.ID})
	})

	mux.HandleFunc("/control/status", func(w http.ResponseWriter, r *http.Request) {
		// Honest empty: no per-agent last-turn status is tracked yet (that's
		// agents/run_agent.py's write_agent_status(), which nothing in this
		// daemon calls). An empty map means the Settings modal just shows no
		// status badges, not an error.
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"status": map[string]interface{}{}})
	})

	mux.HandleFunc("/control/clear", func(w http.ResponseWriter, r *http.Request) {
		board.Clear()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true, "cleared": true})
	})

	mux.HandleFunc("/control/debug", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]bool{"debug": routeDebug.Load()})
			return
		}
		var body struct {
			On *bool `json:"on"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.On != nil {
			routeDebug.Store(*body.On) // explicit set
		} else {
			routeDebug.Store(!routeDebug.Load()) // no field -> toggle (what the /debug slash command sends)
		}
		state := routeDebug.Load()
		log.Printf("[control] routing debug log %s", map[bool]string{true: "ENABLED", false: "disabled"}[state])
		json.NewEncoder(w).Encode(map[string]bool{"ok": true, "debug": state})
	})

	mux.HandleFunc("/control/kick", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Agent string `json:"agent"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if !validID.MatchString(body.Agent) {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "bad agent name"})
			return
		}
		if err := reg.Kill(body.Agent); err != nil {
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]string{"error": "no such agent"})
			return
		}
		rosterRemove(repoRoot, body.Agent)
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "kicked": body.Agent})
	})

	mux.HandleFunc("/control/respawn", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Agent string `json:"agent"`
			CLI   string `json:"cli"` // optional: relaunch this agent on a different backend (Edit dialog)
		}
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		a, ok := reg.Get(body.Agent)
		if !ok {
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]string{"error": "no such agent"})
			return
		}
		id, opts, persona := a.id, a.opts, a.persona
		if body.CLI != "" {
			opts.CLI = body.CLI // change which CLI this agent runs; the respawn below applies it
		}
		reg.Kill(id)
		na, err := reg.Spawn(id, opts, persona)
		if err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "id": na.id})
	})

	mux.HandleFunc("/control/restart", func(w http.ResponseWriter, r *http.Request) {
		board.Post("board", "Restarting the whole crew -- back in a few seconds.", []string{"restart"}, nil)
		n := reg.RestartAll()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "restarting": true, "count": n})
	})

	mux.HandleFunc("/shutdown", func(w http.ResponseWriter, r *http.Request) {
		// EXIT the whole application (board + tray + process) -- the API twin of the
		// tray's "Exit application". Distinct from /control/board?action=stop, which
		// only stops the board and leaves the process alive.
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true, "shutting_down": true})
		go func() {
			time.Sleep(400 * time.Millisecond) // let the response above flush first
			vm.StopSpeaker()                   // /shutdown kills via reg.KillAll directly (not bs.Stop), so stop the speaker here too
			reg.KillAll()
			os.Exit(0)
		}()
	})

	mux.HandleFunc("/control/board", func(w http.ResponseWriter, r *http.Request) {
		// Stop/start the board (HTTP/WS server + crew) WITHOUT killing the process --
		// the API twin of the tray's "Shut down board" / "Start board". NOTE:
		// action=start is only reachable while the board is already up (when it is
		// down, nothing is listening to receive the request), so bringing it back is
		// really the tray's job; this endpoint is mainly a UI "stop" button + tests.
		w.Header().Set("Content-Type", "application/json")
		action := r.URL.Query().Get("action")
		if action == "stop" || action == "start" {
			// State-changing: POST only, so a GET can't stop the board. The global
			// middleware then applies the CSRF header + Origin gate to that POST.
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed: stop/start require POST", http.StatusMethodNotAllowed)
				return
			}
			if action == "stop" {
				json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "action": "stop", "running": false})
				go func() {
					time.Sleep(400 * time.Millisecond) // let the response flush before the server closes
					bs.Stop()
				}()
				return
			}
			err := bs.Start()
			resp := map[string]interface{}{"ok": err == nil, "action": "start", "running": bs.Running()}
			if err != nil {
				resp["error"] = err.Error()
			}
			json.NewEncoder(w).Encode(resp)
			return
		}
		// status (safe GET)
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "running": bs.Running()})
	})

	mux.HandleFunc("/control/tts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			settingsMu.Lock()
			m, mode, active := ttsMuted, voiceMode, time.Since(speakerSeen) <= 30*time.Second
			settingsMu.Unlock()
			json.NewEncoder(w).Encode(map[string]interface{}{"muted": m, "server": active, "mode": mode})
			return
		}
		var body struct {
			Muted     *bool  `json:"muted"`
			Mode      string `json:"mode"`
			Heartbeat bool   `json:"heartbeat"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		settingsMu.Lock()
		if body.Heartbeat {
			speakerSeen = time.Now()
			settingsMu.Unlock()
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "server": true})
			return
		}
		if body.Mode != "" {
			// Allowlist: voiceMode decides whether the browser speech path runs at
			// all, so it is a control value, not free text. An unknown mode would
			// be stored and then read by the UI as "not server-only" -- i.e. it
			// would silently re-enable the browser voices. Reject instead.
			if body.Mode != "auto" && body.Mode != "server-only" {
				settingsMu.Unlock()
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "mode must be auto or server-only"})
				return
			}
			voiceMode = body.Mode
			mSnap := ttsMuted
			settingsMu.Unlock()
			// Persist AFTER unlocking: disk I/O never runs under settingsMu.
			saveVoicePrefs(repoRoot, mSnap, body.Mode)
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "mode": body.Mode})
			return
		}
		changed := body.Muted != nil
		if changed {
			ttsMuted = *body.Muted
		}
		m, mode := ttsMuted, voiceMode
		settingsMu.Unlock()
		if changed {
			// Persist AFTER unlocking: disk I/O never runs under settingsMu.
			saveVoicePrefs(repoRoot, m, mode)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "muted": m})
	})

	mux.HandleFunc("/control/voices", func(w http.ResponseWriter, r *http.Request) {
		settingsMu.Lock()
		assigned := make(map[string]string, len(voiceAssign))
		for k, v := range voiceAssign {
			assigned[k] = v
		}
		settingsMu.Unlock()
		dlState, dlLog, dlErr := vm.DownloadStatus()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"assigned":        assigned,
			"installed":       vm.Installed(),      // are the high-quality (Kokoro) weights present?
			"speaker_running": vm.SpeakerRunning(), // is the daemon-managed speaker up?
			"voices":          kokoroVoices,        // the pickable high-quality voice ids (server speech is the only speech)
			"download":        map[string]string{"state": dlState, "log": dlLog, "error": dlErr},
		})
	})

	mux.HandleFunc("/control/voices/download", func(w http.ResponseWriter, r *http.Request) {
		// Kick the one-time Kokoro weights download (idempotent). POST + CSRF-gated
		// by securityMiddleware. Returns immediately; the UI polls GET /control/voices
		// for progress.
		vm.Download()
		state, logLine, errMsg := vm.DownloadStatus()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "state": state, "log": logLine, "error": errMsg})
	})

	mux.HandleFunc("/control/speaker", func(w http.ResponseWriter, r *http.Request) {
		// Start/stop the high-quality voice speaker (Python sidecar). POST + CSRF-gated.
		var body struct {
			Action string `json:"action"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		switch body.Action {
		case "start":
			if err := vm.StartSpeaker(); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": err.Error()})
				return
			}
		case "stop":
			vm.StopSpeaker()
		default:
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "action must be 'start' or 'stop'"})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "running": vm.SpeakerRunning()})
	})

	mux.HandleFunc("/control/voice", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Agent, Voice string }
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if !validID.MatchString(body.Agent) {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "bad agent name"})
			return
		}
		settingsMu.Lock()
		if body.Voice == "off" {
			delete(voiceAssign, body.Agent)
		} else {
			voiceAssign[body.Agent] = body.Voice
		}
		settingsMu.Unlock()
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "agent": body.Agent, "voice": body.Voice})
	})

	mux.HandleFunc("/control/model", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			settingsMu.Lock()
			m := make(map[string]string, len(modelOverride))
			for k, v := range modelOverride {
				m[k] = v
			}
			settingsMu.Unlock()
			json.NewEncoder(w).Encode(map[string]interface{}{"model": m})
			return
		}
		var body struct{ Agent, Model string }
		json.NewDecoder(r.Body).Decode(&body)
		if !validID.MatchString(body.Agent) {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "bad agent name"})
			return
		}
		settingsMu.Lock()
		if body.Model == "" {
			delete(modelOverride, body.Agent)
		} else {
			modelOverride[body.Agent] = body.Model
		}
		all := make(map[string]string, len(modelOverride))
		for k, v := range modelOverride {
			all[k] = v
		}
		settingsMu.Unlock()
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "agent": body.Agent, "model": body.Model, "all": all})
	})

	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("agent")
		a, ok := reg.Get(id)
		if !ok {
			http.Error(w, "no such agent", http.StatusNotFound)
			return
		}
		serveViewer(w, r, "connected to agent '"+id+"'", func(v *Viewer) func() {
			a.Subscribe(v)
			return func() { a.Unsubscribe(v) }
		}, a.SendPrivatePrompt)
	})

	mux.HandleFunc("/ws/board", func(w http.ResponseWriter, r *http.Request) {
		serveViewer(w, r, "connected to the board", func(v *Viewer) func() {
			agents := reg.All()
			for _, a := range agents {
				a.Subscribe(v)
			}
			return func() {
				for _, a := range agents {
					a.Unsubscribe(v)
				}
			}
		}, nil)
	})

	// Start the board: bind-with-retry + serve in a goroutine + bring the crew up
	// (onStart=bootstrapFleet); onStop=reg.KillAll takes the crew down on a "Shut
	// down board". The bind-retry rationale + the deliberate no-os.Exit-on-serve-
	// error behavior live in lifecycle.go. The INITIAL start is fatal if the port
	// never frees within the retry budget (nothing to run); a later tray "Start
	// board" instead just surfaces the error, since the tray app stays up either way.
	// Wrap the mux in the global CSRF/rebinding gate (see security.go). "Loopback"
	// is not a trust boundary -- the user's browser is a confused deputy -- so every
	// request is Host-checked and every mutation is POST + custom-header + Origin gated.
	bs = newBoardServer("127.0.0.1:"+daemonPort, securityMiddleware(mux),
		func() { bootstrapFleet(repoRoot, reg, board) },
		func() { vm.StopSpeaker(); reg.KillAll() }) // also stop the voice speaker, so a board-stop / Exit / Restart never orphans the Kokoro process
	if err := bs.Start(); err != nil {
		log.Fatalf("[daemon] initial board start failed: %s", err)
	}

	// Speech is server-side only (the browser voices were removed), so if the
	// high-quality voices are installed, bring the speaker up automatically -- then
	// agent voices just work, gated by the 🔊/🔇 mute button. Not installed = silent
	// until the operator downloads them from Settings. Backgrounded so a slow Python
	// start never delays the board.
	go func() {
		if vm.Installed() {
			if err := vm.StartSpeaker(); err != nil {
				log.Printf("[daemon] voices installed but speaker didn't start: %s", err)
			}
		}
	}()

	// Headless mode: no system tray at all. This exists because the tray is the
	// SINGLE biggest source of this daemon dying unexpectedly -- it owns a hidden
	// GUI message loop, and when the process is a background child of a shell
	// whose console gets torn down, that loop receives a close/session event and
	// the tray library fires its Quit callback -> a CLEAN but unwanted shutdown
	// (observed live: a log ending in "[tray] quitting" with nobody having
	// clicked Quit). A headless instance has no such event loop, so a plain
	// backgrounded/detached server survives console teardown. This is the mode
	// the scheduled-task / fleet-up.bat launch path should use. The tray is a
	// nice-to-have convenience for an interactive launch, not part of the
	// server's essential job -- so it's opt-OUT-able, and headless just blocks
	// forever on the already-running server goroutine.
	if os.Getenv("FLEETCHAT_NO_TRAY") != "" {
		log.Printf("[daemon] headless mode (FLEETCHAT_NO_TRAY set) -- no system tray, server only")
		select {} // block forever; the server goroutine above does the real work
	}

	runTray(bs, reg)
}

// announceJoin mirrors run_agent.py's own startup post exactly: intro text
// (from the persona's "intro" field, defaulting the same way loadPersona
// already does), tagged "join". Without this the board looks dead on load
// (no sign any of the 5 real agents are alive) and the @-mention
// autocomplete stays empty (it's seeded from "join"-tagged senders).
func announceJoin(board *Board, id string, persona PersonaConfig) {
	board.Post(id, persona.Intro, []string{"join"}, nil)
}

// bootstrapFleet auto-spawns the REAL configured lineup (data/roster.json,
// the same durable "who's on the team" file run.py itself reads) with their
// REAL personas -- so starting this daemon reproduces the actual crew, not
// an empty board someone has to manually repopulate.
func bootstrapFleet(repoRoot string, reg *Registry, board *Board) {
	entries := readRoster(repoRoot)
	if entries == nil {
		// No durable roster yet (fresh setup, or data/ was wiped). Seed it from a
		// DECLARED crew IF one is configured -- fleet.local.json (git-ignored) or a
		// fleet.json you created -- via the same fleet_file() resolver. A fresh
		// clone has NEITHER, so this seeds nothing and the board boots EMPTY (add
		// agents with "+ Add agent"). An empty "[]" roster (a deliberate kick-all)
		// is distinct from nil here and is left untouched.
		entries = seedRosterFromFleet(repoRoot)
	}
	if len(entries) == 0 {
		log.Printf("[daemon] no roster and no declared crew (fleet.local.json/fleet.json) -- starting with an empty crew")
		return
	}
	for _, e := range entries {
		persona, personaText := loadPersona(repoRoot, e.Name)
		// Where this agent runs FROM (its cwd). A roster entry's own dir (set via
		// the UI folder-picker) wins; otherwise the persona's configured home repo
		// (personas.local/<id>/agent.json "dir"). This is what lands a bootstrapped
		// specialist inside its own repo instead of the daemon dir.
		folder := e.Dir
		if folder == "" {
			folder = persona.Dir
		}
		a, err := reg.Spawn(e.Name, AgentOptions{Persona: personaText, Folder: folder, CLI: persona.CLI}, persona)
		if err != nil {
			log.Printf("[daemon] failed to bootstrap %q: %s", e.Name, err)
			continue
		}
		announceJoin(board, a.id, persona)
		if folder != "" {
			log.Printf("[daemon] bootstrapped %q from the real roster -- running in its own folder %q", e.Name, folder)
		} else {
			log.Printf("[daemon] bootstrapped %q from the real roster (no home folder -- daemon cwd)", e.Name)
		}
	}
}

var agentNamePartRe = regexp.MustCompile(`[^a-z0-9_-]`)

func sanitizeAgentName(folderName string) string {
	return agentNamePartRe.ReplaceAllString(strings.ToLower(folderName), "")
}

// reservedNames are identities the routing/system layer gives special meaning
// to, so no agent may take them and no HTTP caller may post as them: "board"
// is the system-announcement sender (routing.go treats it as never-waking) and
// "all" is the broadcast keyword (routing.go treats it as everyone).
var reservedNames = map[string]bool{"board": true, "all": true}

func isReservedName(id string) bool {
	return reservedNames[strings.ToLower(strings.TrimSpace(id))]
}

// isPrintableASCII reports whether every rune of s is printable ASCII (0x20
// space .. 0x7E '~'). Used to gate the /post sender so an impersonation can't
// hide behind zero-width/control runes or a non-Latin homoglyph. Empty is
// vacuously true; the empty-sender case is rejected separately.
func isPrintableASCII(s string) bool {
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

// isReservedOrKnownAgent reports whether `claimed` belongs to the agent/system
// identity space, so a human/HTTP caller must not post under it. Normalizes
// case+whitespace (agent ids are lowercase) and checks: the reserved names,
// the LIVE registry (covers a /spawn'd agent not in the roster), and the
// DURABLE roster (covers the window while an agent is dead/restarting and thus
// momentarily absent from the live map -- the review's P2 restart-window hole).
func isReservedOrKnownAgent(claimed string, reg *Registry, repoRoot string) bool {
	n := strings.ToLower(strings.TrimSpace(claimed))
	if n == "" {
		return false
	}
	if reservedNames[n] {
		return true
	}
	if _, live := reg.Get(n); live {
		return true
	}
	for _, e := range readRoster(repoRoot) {
		if strings.ToLower(strings.TrimSpace(e.Name)) == n {
			return true
		}
	}
	return false
}

// nativeFolderPicker: PowerShell + Windows Forms, same role as board.py's
// tkinter subprocess -- a real OS dialog, not a browser file input (which
// can't return a directory path, only files).
func nativeFolderPicker() (string, error) {
	// Headless guard (line 1): with no interactive desktop -- a background/service
	// launch -- FolderBrowserDialog opens on an invisible window station and
	// BLOCKS forever. [Environment]::UserInteractive is false there, so exit
	// immediately and let the caller fall back to a typed path instead of hanging
	// the request (the "+ Add agent does nothing" bug).
	script := `if (-not [Environment]::UserInteractive) { exit 3 }
Add-Type -AssemblyName System.Windows.Forms
$f = New-Object System.Windows.Forms.FolderBrowserDialog
$f.Description = "Pick a project folder for the new agent"
if ($f.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { Write-Output $f.SelectedPath }`
	// Bounded even with a desktop: a dialog left open must never pin a request
	// handler indefinitely. A real user picks well within this; past it we error
	// and the UI offers the typed-path fallback.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", script)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// listDirs returns the immediate SUB-FOLDERS of p (names only, sorted), plus the
// resolved absolute path and its parent ("" when at a filesystem root). An empty
// p defaults to the user's home dir. Read-only by construction -- it never opens
// a file, only enumerates directory names -- so the /control/browse picker can
// navigate the tree without exposing any file content.
func listDirs(p string) (dirs []string, cur string, parent string, err error) {
	p = strings.TrimSpace(p)
	if p == "" {
		if h, e := os.UserHomeDir(); e == nil {
			p = h
		} else {
			p = "."
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, "", "", fmt.Errorf("bad path")
	}
	fi, err := os.Stat(abs)
	if err != nil || !fi.IsDir() {
		return nil, "", "", fmt.Errorf("not a folder: %s", abs)
	}
	ents, err := os.ReadDir(abs)
	if err != nil {
		return nil, "", "", fmt.Errorf("can't read that folder (permission?)")
	}
	dirs = []string{}
	for _, e := range ents {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.ToLower(dirs[i]) < strings.ToLower(dirs[j]) })
	if par := filepath.Dir(abs); par != abs {
		parent = par
	}
	return dirs, abs, parent, nil
}

func serveViewer(w http.ResponseWriter, r *http.Request, welcome string, subscribe func(*Viewer) func(), onInput func(string) error) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"localhost:" + daemonPort, "127.0.0.1:" + daemonPort}, // exact port; the global middleware also Host-checks (anti-rebinding)
	})
	if err != nil {
		log.Printf("[ws] accept failed: %s", err)
		return
	}
	ctx := context.Background()
	v := NewViewer(ctx, conn)
	unsubscribe := subscribe(v)
	defer func() {
		unsubscribe()
		v.Close()
		conn.CloseNow()
	}()
	v.Send(NormalizedEvent{Type: "system", Detail: welcome})
	for {
		var msg struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return
		}
		if msg.Type == "input" && msg.Data != "" && onInput != nil {
			_ = onInput(msg.Data)
		}
	}
}
