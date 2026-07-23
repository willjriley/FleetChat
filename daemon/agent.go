package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// NormalizedEvent is the daemon's OWN internal shape -- deliberately not just
// Claude's raw event names, so a later Gemini/Qwen/Codex adapter can produce
// this exact same shape from its own (differently-named) stream-json events.
// AgentID is what makes the SAME event usable for both a private 1:1 view
// (subscribe to one agent) and a public board view (subscribe to all of
// them, same shape, just a wider audience) -- the "same feed, different
// scope" design from tonight's earlier conversation, not two systems.
type NormalizedEvent struct {
	AgentID string `json:"agentId"`
	Type    string `json:"type"`              // "thinking" | "message" | "done" | "error" | "rate_limit" | "system"
	Text    string `json:"text,omitempty"`    // populated for "message"
	Partial bool   `json:"partial,omitempty"` // true for in-progress "thinking" chunks
	Detail  string `json:"detail,omitempty"`  // free-form extra info (e.g. rate-limit message)
}

// rawClaudeLine is only the fields we need to route each line -- Claude's
// actual schema has more, we deliberately don't model all of it here.
type rawClaudeLine struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	// SessionID arrives on the system/init line. It was previously parsed off
	// and thrown away -- route() noted "session started" and discarded the one
	// value needed to resume that exact conversation later. Capturing it is
	// what makes restart-survival possible at all (see sessions.go).
	SessionID string `json:"session_id"`
	Message *struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

type Agent struct {
	id        string
	opts      AgentOptions  // remembered so a restart can respawn with the SAME model/persona, not silently reset to defaults
	persona   PersonaConfig // remembered so /roster and a tray restart can show/reuse the real name+role, not just the raw id
	cmd       *exec.Cmd
	in        *bufio.Writer // subprocess stdin, wrapped for line-writing
	mu        sync.Mutex
	subs      map[*Viewer]bool
	buf       *ringBuffer                   // reconnect-backfill, see ringbuffer.go
	onExit    func()                        // set by the registry: cleans up bookkeeping if the process dies on its OWN
	onMessage func(agentID, text string)    // set by the registry: feeds this agent's replies back into the shared Board
	onTyping  func(agentID string, on bool) // set by the registry: drives GET /typing's animated "…"
	// onSession fires once per process, when system/init reports the claude
	// session id. The registry persists it so the NEXT spawn of this agent id
	// can --resume this exact conversation. Guarded by a.mu like the others.
	onSession func(agentID, sessionID string)
	// sessionID is this process's live session, "" until init arrives. Also
	// serves as the resume-succeeded signal: still "" means init never came.
	sessionID string
	// pendingPrivate is a FIFO queue, not a single flag: this process can have
	// MORE than one turn in flight (a board reply and a private reply sent
	// close together both queue on the same stdin), and "result" events
	// resolve in the same order turns were sent (the CLI processes queued
	// stdin turns strictly sequentially, never interleaved). Pushed at send
	// time, peeked by "assistant" events to decide the board echo, popped at
	// "result" once that turn is fully resolved. A single bool here tagged
	// the WRONG turn under exactly this overlap: turn 1's board reply could
	// read turn 2's private flag (suppressing a real board reply) and vice
	// versa (leaking a private reply onto the board) -- the exact bug the operator
	// reported, that a single isolated test could never catch.
	pendingPrivate []bool
	// taught records whether this agent has already been taught the board rules.
	// The board prepends the full ruleset to an agent's turn only while this is
	// false (then sets it), so a persistent agent -- whose context carries the
	// rules across turns -- is taught ONCE, not on every message. Rules never
	// change under a running agent: a rules change means restarting the board,
	// which makes fresh agents that get taught anew. Guarded by a.mu.
	taught bool
}

// AgentOptions is deliberately just two optional strings today, not a full
// per-CLI config -- Model and Persona are both meaningful the same way for
// any backend that shares Claude's flag shape. A real Gemini/Qwen adapter
// would translate these into ITS OWN flags rather than assuming --model/
// --system-prompt are universal.
type AgentOptions struct {
	Model   string // "" = whatever the claude CLI's own default is
	Persona string // "" = claude's own default persona; replaces it wholesale, same as FleetChat's own claude_reply() pattern
	Folder  string // "" = no project folder; matches run_agent.py's FLEETCHAT_AGENT_DIR / --add-dir
	// ResumeSession is this agent's OWN prior claude session id ("" = start
	// fresh). Set from data/sessions.json on respawn, which is what makes an
	// agent survive a board restart with its memory intact. Per-agent by
	// construction -- see sessions.go for why the --continue flag cannot be
	// used here without agents inheriting each other's conversations.
	ResumeSession string
}

// NewAgent starts the subprocess and builds the Agent, but deliberately does
// NOT start readLoop -- the caller (Registry.Spawn) must finish setting
// persona/onExit/onMessage/onTyping and only THEN call Start(). Those fields
// used to get set after `go a.readLoop(stdout)` had already been kicked off
// here, which meant a real (if narrow -- subprocess launch latency almost
// always hides it) data race: a fast-starting process could reach route()'s
// first event and read onMessage/onTyping while Spawn was still in the
// middle of assigning them, with neither side holding a.mu.
func NewAgent(id string, opts AgentOptions) (*Agent, io.Reader, error) {
	args := []string{
		"-p",
		"--input-format=stream-json",
		"--output-format=stream-json",
		"--include-partial-messages",
		"--verbose",
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.Persona != "" {
		args = append(args, "--system-prompt", opts.Persona)
	}
	if opts.Folder != "" {
		args = append(args, "--add-dir", opts.Folder)
	}
	// Resume THIS agent's own prior conversation. Validated before use because
	// it reaches the child as an argv element and the file it came from is
	// hand-editable; an id that fails the shape check is dropped and the agent
	// starts fresh rather than being passed through to the CLI.
	if opts.ResumeSession != "" && validSessionID.MatchString(opts.ResumeSession) {
		args = append(args, "--resume", opts.ResumeSession)
	}
	claudeBin := "claude"
	if env := os.Getenv("FLEETCHAT_CLAUDE"); env != "" {
		claudeBin = env // matches run_agent.py's own override -- a scheduled-task/service launch context may not have "claude" resolvable on PATH at all
	}
	cmd := exec.Command(claudeBin, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start claude: %w", err)
	}

	a := &Agent{
		id:   id,
		opts: opts,
		cmd:  cmd,
		in:   bufio.NewWriter(stdin),
		subs: make(map[*Viewer]bool),
		buf:  newRingBuffer(ringBufferMaxBytes),
	}

	// Was previously left nil (cmd.Stderr) and silently discarded -- a crash
	// showed up in the logs as only "read loop ended (EOF)" with no cause.
	// Streamed straight to the daemon's own log rather than buffered: this
	// process can run for the agent's whole lifetime, so an in-memory buffer
	// would grow unbounded.
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			log.Printf("[agent %s stderr] %s", id, scanner.Text())
		}
	}()

	return a, stdout, nil
}

// Start begins reading the subprocess's output. Call it only after every
// field readLoop/route() touch (persona, onExit, onMessage, onTyping) is
// already set -- see NewAgent's doc comment.
func (a *Agent) Start(stdout io.Reader) {
	go a.readLoop(stdout)
}

// readLoop is the whole point of this file: turn Claude's raw stream-json
// lines into the daemon's normalized events, and broadcast each one.
func (a *Agent) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // long lines are normal here
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var raw rawClaudeLine
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			log.Printf("[agent %s] unparseable line (skipped): %s", a.id, truncate(line, 120))
			continue
		}
		a.route(raw, line)
	}
	// The process is gone -- whether it crashed, hit an unrecoverable auth
	// error, or was deliberately killed, stdout closing means it's not
	// coming back. Tell viewers (a real event, not silence -- the same
	// dead-man's-switch principle tonight's other work leaned on all
	// night) and make sure the registry's bookkeeping actually reflects
	// that, instead of listing a dead agent as alive indefinitely.
	if err := scanner.Err(); err != nil {
		log.Printf("[agent %s] read loop ended with error: %s", a.id, err)
	} else {
		log.Printf("[agent %s] read loop ended (EOF)", a.id)
	}
	a.broadcast(NormalizedEvent{AgentID: a.id, Type: "removed", Detail: "process exited"})
	a.clearTyping() // safety net: a crash mid-turn may never emit "result" at all
	if a.onExit != nil {
		a.onExit()
	}
}

func (a *Agent) route(raw rawClaudeLine, rawLine string) {
	switch raw.Type {
	case "system":
		if raw.Subtype == "init" {
			// init is also the "this process is genuinely live" signal, which is
			// what lets Registry tell a successful resume from a rejected one:
			// a --resume against an expired id dies BEFORE emitting init.
			a.mu.Lock()
			a.sessionID = raw.SessionID
			cb := a.onSession
			a.mu.Unlock()
			if cb != nil && raw.SessionID != "" {
				cb(a.id, raw.SessionID)
			}
			a.broadcast(NormalizedEvent{AgentID: a.id, Type: "system", Detail: "session started"})
		}
	case "rate_limit_event":
		a.broadcast(NormalizedEvent{AgentID: a.id, Type: "rate_limit", Detail: rawLine})
	case "stream_event":
		a.broadcast(NormalizedEvent{AgentID: a.id, Type: "thinking", Partial: true})
	case "assistant":
		if raw.Message != nil {
			private := a.peekPendingPrivate()
			for _, c := range raw.Message.Content {
				if c.Type != "text" || c.Text == "" {
					continue
				}
				a.broadcast(NormalizedEvent{AgentID: a.id, Type: "message", Text: c.Text})
				// Faithful port of run_agent.py's exact PASS check: uppercase,
				// strip a trailing "."/"!", compare to "PASS" -- a PASS is real
				// output (still shown in the private 1:1 view above) but never
				// feeds back into the shared board, or every quiet agent would
				// visibly clutter it every single turn. A privately-triggered
				// turn skips the board echo entirely, PASS or not -- "private"
				// means private.
				if a.onMessage != nil && !private && !isPass(c.Text) {
					a.onMessage(a.id, c.Text)
				}
			}
		}
	case "result":
		a.broadcast(NormalizedEvent{AgentID: a.id, Type: "done", Detail: raw.Subtype})
		// Mirrors run_agent.py's `finally: board.set_typing(id, False)` -- the turn is over
		// whether it succeeded or errored, either way "result" is Claude's own signal for that.
		// Only clear once NO turn is left in flight: if a board reply and a private
		// reply overlap, the first one to resolve must not hide the "…" for the
		// other, which is still generating.
		if stillInFlight := a.popPendingPrivate(); !stillInFlight && a.onTyping != nil {
			a.onTyping(a.id, false)
		}
	default:
		// Deliberately silent: tool_use/tool_result and anything else not
		// modeled yet. Not an error -- just not surfaced to viewers today.
	}
}

// SendPrompt feeds a new user turn into the ALREADY-RUNNING process --
// this is the persistence property proven in tonight's spike, not a new
// process per call. The reply is forwarded to the shared board (onMessage)
// like any other turn.
func (a *Agent) SendPrompt(text string) error {
	return a.sendPrompt(text, false)
}

// SendPrivatePrompt is the SAME persistent process, SAME conversation --
// just triggered from the private 1:1 view (/ws?agent=<id>) instead of the
// board. The one difference: route() skips the onMessage board-echo for the
// reply this produces, so a private question doesn't leak onto the public
// feed. The operator's own read after testing the first version: private chat "was
// also posted to the public channel" -- not what "private" should mean.
func (a *Agent) SendPrivatePrompt(text string) error {
	return a.sendPrompt(text, true)
}

func (a *Agent) sendPrompt(text string, private bool) error {
	// Mirrors run_agent.py's `board.set_typing(id, True)` right before the claude call --
	// set BEFORE the write below, not after, so the UI's "…" can never lag the real state.
	if a.onTyping != nil {
		a.onTyping(a.id, true)
	}
	msg := map[string]interface{}{
		"type": "user",
		"message": map[string]interface{}{
			"role": "user",
			"content": []map[string]interface{}{
				{"type": "text", "text": text},
			},
		},
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// Push BEFORE the write, in the same order the write happens -- the queue
	// order must match the order turns actually reach the CLI's stdin.
	a.pendingPrivate = append(a.pendingPrivate, private)
	if _, err := a.in.Write(b); err != nil {
		a.undoPendingPrivateLocked() // this turn never actually reached the process -- don't leave a phantom queue entry
		a.clearTyping()
		return err
	}
	if err := a.in.WriteByte('\n'); err != nil {
		a.undoPendingPrivateLocked()
		a.clearTyping()
		return err
	}
	if err := a.in.Flush(); err != nil {
		a.undoPendingPrivateLocked()
		a.clearTyping()
		return err
	}
	return nil
}

// undoPendingPrivateLocked removes the entry sendPrompt just pushed, when the
// write that was supposed to correspond to it failed. Caller must hold a.mu.
// Safe to assume it's the LAST element: a.mu has been held continuously since
// the push, so nothing else could have pushed or popped in between.
func (a *Agent) undoPendingPrivateLocked() {
	if n := len(a.pendingPrivate); n > 0 {
		a.pendingPrivate = a.pendingPrivate[:n-1]
	}
}

// peekPendingPrivate reports whether the turn CURRENTLY producing assistant
// output is private, without consuming it -- a turn can emit several
// "assistant" events before its "result". Relies on one invariant: every
// sent turn eventually produces exactly one "result" (including the error
// subtype), which is what keeps push/pop counts matched. If that's ever
// violated the queue desyncs PERMANENTLY (off by one for the rest of the
// process's life, unlike the single-bool this replaced, which self-healed
// once an overlap passed) -- there's no turn-id in the modeled fields to
// self-correct from, so an empty peek here is the earliest visible symptom
// and worth a log line rather than silently defaulting.
func (a *Agent) peekPendingPrivate() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.pendingPrivate) == 0 {
		log.Printf("[agent %s] pendingPrivate desync: assistant event with no queued turn -- a turn may have completed without emitting a matching result", a.id)
		return false // never misattribute on the safe side
	}
	return a.pendingPrivate[0]
}

// popPendingPrivate retires the turn that just produced "result" -- called
// exactly once per turn, so the queue never grows unbounded. Returns
// whether any OTHER turn is still queued behind it: the queue's length is
// already exactly "how many turns are outstanding right now" (pushed at
// send, popped at result), so it doubles as the typing in-flight count for
// free -- see route()'s "result" case, which uses this to avoid clearing
// the "…" indicator while a second overlapping turn is still generating.
func (a *Agent) popPendingPrivate() (stillInFlight bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.pendingPrivate) > 0 {
		a.pendingPrivate = a.pendingPrivate[1:]
	}
	return len(a.pendingPrivate) > 0
}

func (a *Agent) clearTyping() {
	if a.onTyping != nil {
		a.onTyping(a.id, false)
	}
}

// NeedsTeaching reports whether this agent has NOT yet been taught the board
// rules. The board prepends the full ruleset to an agent's turn only when this
// is true, then calls MarkTaught -- so a persistent agent (whose context
// carries the rules forward) is taught once, not every message.
func (a *Agent) NeedsTeaching() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.taught
}

// MarkTaught records that this agent has now been taught the board rules. A
// rare double-teach (two turns to one agent both seeing NeedsTeaching before
// either marks) is harmless -- it just re-sends the rules once more.
func (a *Agent) MarkTaught() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.taught = true
}

// Subscribe replays the buffered backlog to v BEFORE registering it for live
// events -- a reconnecting viewer catches up on what it missed, then goes
// live, same order ClaudeCanvas's own docs describe (flush buffers, then the
// agent is live for that viewer). Locking the agent mutex for the whole
// replay is deliberate: it means no NEW event can be broadcast (and thus
// missed by v) while the backlog is still being sent, at the cost of briefly
// blocking other viewers' broadcasts during a reconnect -- an acceptable
// trade for a skeleton; a real implementation might snapshot+release instead.
func (a *Agent) Subscribe(v *Viewer) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.buf.Snapshot() {
		v.Send(e)
	}
	a.subs[v] = true
}

func (a *Agent) Unsubscribe(v *Viewer) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.subs, v)
}

// Kill terminates the subprocess. It does NOT broadcast "removed" itself --
// killing the process closes its stdout, which readLoop already treats as
// "this agent is gone" (see the end of readLoop) and handles exactly once,
// the same way whether the process was deliberately killed or crashed on
// its own. One code path owning that notification, not two -- duplicating
// it here would just re-create the split-bookkeeping shape tonight's real
// bug came from, in miniature.
func (a *Agent) Kill() error {
	if a.cmd.Process == nil {
		return nil
	}
	return a.cmd.Process.Kill()
}

// broadcast ALWAYS buffers, live viewers or not -- output while nobody's
// watching is exactly the case the ring buffer exists for.
func (a *Agent) broadcast(e NormalizedEvent) {
	b, _ := json.Marshal(e) // approximate wire size for the buffer's byte budget
	a.mu.Lock()
	a.buf.Add(e, len(b))
	viewers := make([]*Viewer, 0, len(a.subs))
	for v := range a.subs {
		viewers = append(viewers, v)
	}
	a.mu.Unlock()
	for _, v := range viewers {
		v.Send(e)
	}
}

func isPass(s string) bool {
	t := strings.ToUpper(strings.TrimRight(strings.TrimSpace(s), ".!"))
	return t == "PASS"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
