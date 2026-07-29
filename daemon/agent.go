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
	"sync/atomic"
	"time"
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
	Message   *struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

type Agent struct {
	id        string
	opts      AgentOptions // remembered so a restart can respawn with the SAME model/config, not silently reset to defaults
	info      AgentInfo    // remembered so /roster and a tray restart can show/reuse the agent's name + run config, not just the raw id
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
	// onActivity is a liveness ping, NOT a state change: every event from this
	// agent refreshes its typing entry's TTL so a long-running turn keeps its
	// "…". It only refreshes an entry that already exists (see TouchTyping).
	onActivity func(agentID string)
	// exited is closed once this agent's process is CONFIRMED gone: stdout
	// drained, stderr drained, and cmd.Wait() returned. Kill() blocks on it so
	// "killed" means "actually dead", not "signal delivered".
	exited chan struct{}
	// stderrDone is closed when the stderr scanner finishes. cmd.Wait() closes
	// the pipes, and the os/exec docs are explicit that calling Wait before all
	// pipe reads have completed is a race -- so Wait happens only after BOTH
	// readers are done.
	stderrDone chan struct{}
	// dying marks an agent whose Kill is underway. Spawn refuses to hand back a
	// dying agent, so an id cannot be reused while its previous process is
	// still tearing down. Atomic rather than mutex-guarded to avoid nesting a
	// second lock inside the registry's.
	dying atomic.Bool
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
	// sendCh decouples "hand a turn to this agent" from the actual stdin WRITE.
	// sendPrompt enqueues here (non-blocking); the agent's own sendLoop drains it
	// and does the write. This is what stops a wedged recipient -- one not draining
	// its stdin -- from blocking the CALLER, which for a board reply is the SENDER's
	// readLoop: the head-of-line stall that once froze a sender's turn AND hung
	// shutdown. A stuck recipient now backs up only its own buffered queue.
	sendCh chan sendJob
}

// sendJob is one queued turn awaiting the agent's stdin (see sendCh / sendLoop).
type sendJob struct {
	text    string
	private bool
}

// AgentOptions carries a single agent's launch config. Model/Folder are
// expressed in Claude's flag shape; CLI selects WHICH backend turns them into
// its own flags (see buildCLICommand) -- a real Gemini/Qwen adapter emits its
// own, not an assumption that --model/--system-prompt are universal.
type AgentOptions struct {
	// ExtraArgs are appended verbatim to the built command. Operator-supplied,
	// per agent, so a CLI flag we have never heard of still works -- including
	// one added by a future release of that CLI.
	ExtraArgs []string
	Model     string // "" = whatever the CLI's own default is
	Folder    string // "" = no home folder; otherwise the agent's cwd (its own repo) + --add-dir
	// CLI picks which backend launches this agent: "claude" (default when "") |
	// "gemini" | "qwen". Per-agent, so the board can run different CLIs in
	// different folders. Set in the Add/Edit dialog and stored in the roster. Only
	// the claude profile is fully wired today; see buildCLICommand.
	CLI string
	// ResumeSession is this agent's OWN prior claude session id ("" = start
	// fresh). Set from data/sessions.json on respawn, which is what makes an
	// agent survive a board restart with its memory intact. Per-agent by
	// construction -- see sessions.go for why the --continue flag cannot be
	// used here without agents inheriting each other's conversations.
	ResumeSession string
	// FullPermissions runs this agent with the approval gate OFF (claude
	// --dangerously-skip-permissions / qwen --approval-mode yolo): it acts without
	// asking first. A per-agent OPT-IN, so skipping the prompts stays a deliberate
	// choice rather than the default.
	//
	// It is NOT a sandbox toggle, and leaving it off is NOT a path restriction.
	// --add-dir ADDS a directory to what the CLI treats as in-scope; it confines
	// nothing. Verified: an agent with this off read a sibling agent's repo, wrote
	// outside its own folder, and opened an SSH session to another machine -- the
	// approval prompts were the only difference. The UI used to describe off as
	// "scoped to its own folder"; that claim is gone because it was never true.
	FullPermissions bool
}

// agentWorkDir resolves the cwd an agent should run in: its own project folder
// when that folder exists on disk, else "" (which exec treats as "inherit the
// daemon's cwd"). Validated + isolated so a bad or relative folder can't reach
// cmd.Dir and make cmd.Start() fail outright -- a set-but-missing folder is
// logged and the agent still spawns (from the daemon dir) rather than vanishing.
func agentWorkDir(id, folder string) string {
	if folder == "" {
		return ""
	}
	if fi, err := os.Stat(folder); err == nil && fi.IsDir() {
		return folder
	}
	log.Printf("[agent %s] configured folder %q is not a usable directory -- running from the daemon cwd instead", id, folder)
	return ""
}

// buildCLICommand turns per-agent options into the (binary, args) for THAT
// agent's chosen CLI -- the multi-CLI seam. Each agent's config picks its cli
// ("claude" | "gemini" | "qwen"), so the board can run Claude in one repo,
// Gemini in another, and Qwen in a third, each launched with its own command.
//
// Today only the claude profile is fully wired: its stream-json flags here AND
// the rawClaudeLine output adapter that route() parses. gemini/qwen are
// recognized backends whose arg + output-stream adapters are not built yet, so
// selecting one fails LOUDLY rather than launching claude's flags at a different
// binary (which would misbehave silently). Adding a backend is one more case
// here plus its output adapter -- not a rewrite. "" defaults to claude.
// splitArgs turns the operator's single free-text argument line into an argv
// slice. Whitespace separates; quotes group; a backslash is LITERAL except when
// it immediately precedes a quote character, where it escapes that quote.
//
// That rule is not invented here -- it is the Windows CommandLineToArgvW
// convention -- and it is the only common rule that leaves `C:\repos\forge`
// intact whether or not the operator quoted it. POSIX shell rules would turn
// that same unquoted path into `C:reposforge` silently, which is the worst
// available failure for a field whose entire point is that you can see what
// will run. On Linux and macOS the practical difference is narrow: quoting for
// spaces behaves identically, and only the rarer `foo\ bar` idiom has to be
// written as "foo bar" instead.
//
// Splitting a line at all is a real limitation of a one-line field -- most
// tools dodge it by taking a JSON array (VS Code, Docker, Kubernetes) or by
// handing the string to an actual shell. This takes the third road, systemd's:
// its own documented rule, plus the dialog rendering the RESULT token by token,
// so the operator never has to know the rule to see what it produced.
//
// No shell is involved -- these go to exec directly -- so `;` and `|` arrive as
// ordinary characters inside an argument, not as operators.
func splitArgs(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune // 0 = not inside quotes, else the opening quote character
	started := false

	flush := func() {
		if started {
			out = append(out, cur.String())
			cur.Reset()
			started = false
		}
	}
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		// A backslash escapes ONLY a quote character. Anywhere else it is a
		// literal, which is what keeps an unquoted Windows path intact.
		if r == '\\' && i+1 < len(rs) && (rs[i+1] == '"' || rs[i+1] == '\'') {
			cur.WriteRune(rs[i+1])
			started = true
			i++
			continue
		}
		switch {
		case quote != 0:
			if r == quote {
				quote = 0 // closing quote: the argument continues, e.g. --dir="a b"x
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
			started = true // `""` is a real (empty) argument, so mark it started here
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	flush() // an unterminated quote yields the rest as one argument rather than dropping it
	return out
}

// appendExtra puts operator-supplied arguments LAST, so they can override an
// earlier flag where the CLI honours last-wins, and so the preview shows them
// in the position they will actually occupy.
func appendExtra(args []string, extra []string) []string {
	for _, a := range extra {
		if strings.TrimSpace(a) != "" {
			args = append(args, a)
		}
	}
	return args
}

func buildCLICommand(opts AgentOptions) (bin string, args []string, err error) {
	// ExtraArgs are applied HERE, once, rather than inside each backend: a new
	// backend added later cannot forget to honour them, and the preview endpoint
	// calls this same function so what is shown is what runs.
	switch cli := strings.ToLower(strings.TrimSpace(opts.CLI)); cli {
	case "", "claude":
		bin, args = claudeCommand(opts)
		return bin, appendExtra(args, opts.ExtraArgs), nil
	case "qwen":
		// NOT appendExtra: qwenCommand already forwarded them as --extra-arg
		// pairs for the adapter to hand to qwen itself.
		return qwenCommand(opts)
	case "gemini":
		// INVARIANT when you wire this: every managed agent MUST receive protocolRules()
		// via this backend's own system-prompt equivalent (claude uses --append-system-prompt;
		// qwen prepends it inside the adapter). Skip it and a managed agent on this CLI runs with
		// NO board trust boundary. Also re-verify that mechanism does NOT persist into a resumed
		// session in a way that re-taints a standalone launch.
		return "", nil, fmt.Errorf("cli %q is a recognized backend but its adapter isn't wired yet -- only claude and qwen are implemented today", cli)
	default:
		return "", nil, fmt.Errorf("unknown cli %q (want one of: claude, gemini, qwen)", cli)
	}
}

// claudeCommand builds the claude CLI invocation -- the one fully-wired profile.
// Kept separate from buildCLICommand so each future backend is its own peer
// function with its own flag vocabulary, not a pile of conditionals.
func claudeCommand(opts AgentOptions) (bin string, args []string) {
	args = []string{
		"-p",
		"--input-format=stream-json",
		"--output-format=stream-json",
		"--include-partial-messages",
		"--verbose",
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.Folder != "" {
		args = append(args, "--add-dir", opts.Folder)
	}
	if opts.FullPermissions {
		// Drops claude's APPROVAL PROMPTS. It does not change which paths the agent
		// can reach.
		//
		// This comment used to say the agent could then act "on ANY path ... not
		// just its --add-dir folder", which implied --add-dir was a boundary. It is
		// not: --add-dir ADDS a directory to the working set, it does not confine
		// the agent to it. Verified empirically -- an agent with FullPermissions
		// FALSE read a sibling agent's repo, wrote to a third directory, read the
		// user's ~/.ssh, and opened an SSH session to another machine.
		//
		// So: OFF means prompts, not containment. Real confinement is a property of
		// the environment the agent runs in (a container with only its own volume),
		// not of a flag. Do not restore wording that implies otherwise.
		args = append(args, "--dangerously-skip-permissions")
	}
	// Deliver the board rulebook as an appended system prompt at LAUNCH, not as a
	// conversation message. Supplied fresh on every daemon launch, it never enters
	// the agent's saved session -- so a hand-launched or resumed `claude` in this repo
	// has no board rules to act on (no "taint"). This is board PROTOCOL (how to drive
	// the board API), NOT an identity/persona: the agent's identity stays its own
	// home-repo CLAUDE.md, with nothing layered over it.
	args = append(args, "--append-system-prompt", protocolRules())
	// Resume THIS agent's own prior conversation. Validated before use because it
	// reaches the child as an argv element and the file it came from is
	// hand-editable; an id that fails the shape check is dropped and the agent
	// starts fresh rather than being passed through to the CLI.
	if opts.ResumeSession != "" && validSessionID.MatchString(opts.ResumeSession) {
		args = append(args, "--resume", opts.ResumeSession)
	}
	bin = "claude"
	if env := os.Getenv("FLEETCHAT_CLAUDE"); env != "" {
		bin = env // matches run_agent.py's override -- a service/scheduled-task launch may not have "claude" on PATH
	}
	return bin, args
}

// qwenCommand launches THIS daemon binary in qwen-adapter mode as the agent's
// held-open process. Qwen-Code is one-shot per -p (no held-open stdin protocol
// like claude), so the adapter (qwen_adapter.go) does the spawn-per-turn calls and
// speaks the daemon's stream-json contract -- keeping all qwen-specific logic out
// of the daemon core. Model is deliberately unset (qwen uses its own default).
func qwenCommand(opts AgentOptions) (bin string, args []string, err error) {
	exe, err := os.Executable()
	if err != nil {
		return "", nil, fmt.Errorf("qwen adapter: cannot resolve daemon executable: %w", err)
	}
	args = []string{"qwen-adapter"}
	if opts.Folder != "" {
		args = append(args, "--repo", opts.Folder)
	}
	if opts.ResumeSession != "" && validSessionID.MatchString(opts.ResumeSession) {
		args = append(args, "--resume", opts.ResumeSession)
	}
	if qb := os.Getenv("FLEETCHAT_QWEN"); qb != "" {
		args = append(args, "--qwen-bin", qb) // parity with FLEETCHAT_CLAUDE
	}
	if opts.FullPermissions {
		args = append(args, "--full-perms") // adapter turns this into qwen's own approval-bypass flag
	}
	// Operator arguments are FORWARDED, not appended: what buildCLICommand
	// returns here is the ADAPTER's command line, and qwen is the adapter's
	// child. Appending them to this argv would give them to the wrapper.
	for _, a := range opts.ExtraArgs {
		if strings.TrimSpace(a) != "" {
			args = append(args, "--extra-arg", a)
		}
	}
	return exe, args, nil
}

// NewAgent starts the subprocess and builds the Agent, but deliberately does
// NOT start readLoop -- the caller (Registry.Spawn) must finish setting
// info/onExit/onMessage/onTyping/onActivity and only THEN call Start(). Those fields
// used to get set after `go a.readLoop(stdout)` had already been kicked off
// here, which meant a real (if narrow -- subprocess launch latency almost
// always hides it) data race: a fast-starting process could reach route()'s
// first event and read onMessage/onTyping while Spawn was still in the
// middle of assigning them, with neither side holding a.mu.
func NewAgent(id string, opts AgentOptions) (*Agent, io.Reader, error) {
	// Build the launch command for THIS agent's chosen CLI -- the multi-CLI seam.
	bin, args, err := buildCLICommand(opts)
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.Command(bin, args...)
	// Must happen BEFORE Start: SysProcAttr is read at fork time, so setting it
	// afterwards is silently ignored. On POSIX this makes the child its own
	// process-group leader, which is the whole precondition killProcessTree
	// relies on -- without it kill(-pid) names no existing group, returns
	// ESRCH, and the "tree kill" quietly does nothing while the CLI's children
	// survive as orphans. No-op on Windows, where taskkill /T walks the real
	// parent/child links instead.
	configureProcessGroup(cmd)
	// THE BOARD'S CORE DESIGN: each agent is a specialist that runs FROM ITS OWN
	// repo. Setting cmd.Dir is what makes that real -- the agent's cwd, its
	// relative paths, and its per-project CLAUDE.md / .claude/settings.local.json
	// all resolve inside its folder, not the daemon's dir. --add-dir (above) only
	// grants tool ACCESS to the folder; without cmd.Dir every agent would still
	// run from the daemon dir and share one cwd. Must be set before Start (cmd.Dir
	// is read at fork time). Validated in agentWorkDir so a bad path can't fail
	// the spawn.
	cmd.Dir = agentWorkDir(id, opts.Folder)
	// Board-managed signal: mark this as a process the FleetChat daemon launched, so
	// the agent can tell "I'm the live board instance" from "someone ran `claude` by
	// hand in this repo." A hand-launch never carries these; the board rules gate on
	// FLEETCHAT_MANAGED so a stray or manually-resumed session stands down instead of
	// posting into a board nobody is driving. Set before Start (like cmd.Dir, the
	// environment is captured at fork time).
	cmd.Env = append(os.Environ(), "FLEETCHAT_MANAGED=1", "FLEETCHAT_AGENT="+id)
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
		id:         id,
		opts:       opts,
		cmd:        cmd,
		in:         bufio.NewWriter(stdin),
		subs:       make(map[*Viewer]bool),
		buf:        newRingBuffer(ringBufferMaxBytes),
		exited:     make(chan struct{}),
		stderrDone: make(chan struct{}),
		// Buffered so a recipient that's momentarily busy (mid-turn, not reading
		// stdin) queues rather than back-pressuring the sender. A full queue means
		// genuinely stuck -> sendPrompt reports it instead of blocking. 64 is far
		// more than the handful of turns a healthy agent buffers during one reply.
		sendCh: make(chan sendJob, 64),
	}

	// Was previously left nil (cmd.Stderr) and silently discarded -- a crash
	// showed up in the logs as only "read loop ended (EOF)" with no cause.
	// Streamed straight to the daemon's own log rather than buffered: this
	// process can run for the agent's whole lifetime, so an in-memory buffer
	// would grow unbounded.
	go func() {
		defer close(a.stderrDone) // readLoop waits on this before cmd.Wait()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			log.Printf("[agent %s stderr] %s", id, scanner.Text())
		}
	}()

	return a, stdout, nil
}

// Start begins reading the subprocess's output. Call it only after every
// field readLoop/route() touch (info, onExit, onMessage, onTyping, onActivity) is
// already set -- see NewAgent's doc comment.
func (a *Agent) Start(stdout io.Reader) {
	go a.readLoop(stdout)
	go a.sendLoop() // writes this agent's queued turns to stdin -- see sendCh
}

// readLoop is the whole point of this file: turn Claude's raw stream-json
// lines into the daemon's normalized events, and broadcast each one.
func (a *Agent) readLoop(stdout io.Reader) {
	// Panic containment: everything downstream of route() (onMessage -> board.Post
	// -> recipient enqueues, viewer sends, ...) runs on THIS goroutine. An
	// uncontained panic there would unwind readLoop WITHOUT closing exited --
	// stranding Kill (10s timeout) and, since the send-queue change, leaking this
	// agent's sendLoop forever. Recover and still perform the ORDERLY reap
	// (stderrDone -> Wait -> close); a bare `defer close(a.exited)` would be wrong,
	// as exited means "dead AND reaped" (Spawn reuses the id on it). reaped guards
	// the double-close if the panic strikes after the normal-path close below.
	reaped := false
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		log.Printf("[agent %s] PANIC in read loop (contained): %v", a.id, r)
		if !reaped {
			<-a.stderrDone // returns immediately once the stderr scanner is done (closed channel)
			_ = a.cmd.Wait()
			close(a.exited)
		}
		a.clearTyping()
		if a.onExit != nil {
			a.onExit()
		}
	}()
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
	// REAP the process. Without this the exit status is never collected: on
	// POSIX that leaves a zombie, on Windows it leaks the process handle, and
	// either way it accumulates across every restart in a long-lived daemon.
	//
	// Order matters and is why stderrDone exists. cmd.Wait() closes the pipes,
	// and os/exec documents that calling it before all pipe reads have finished
	// is a race -- stdout is drained (the scanner loop above just ended) and
	// this waits for stderr before reaping.
	<-a.stderrDone
	if werr := a.cmd.Wait(); werr != nil && !a.dying.Load() {
		// A non-zero exit is expected when WE killed it; only surface it when
		// the process died on its own.
		log.Printf("[agent %s] exited: %s", a.id, werr)
	}
	// Publish "confirmed dead" only after the reap, so anything waiting on
	// exited (Kill) is guaranteed the process is really gone and its id is
	// safe to reuse.
	close(a.exited)
	reaped = true // the panic-recovery defer above must not close it again

	a.broadcast(NormalizedEvent{AgentID: a.id, Type: "removed", Detail: "process exited"})
	a.clearTyping() // safety net: a crash mid-turn may never emit "result" at all
	if a.onExit != nil {
		a.onExit()
	}
}

func (a *Agent) route(raw rawClaudeLine, rawLine string) {
	// ANY event is proof this agent is still working -- refresh its typing TTL
	// before dispatching, so a slow turn never has its "…" reaped mid-flight.
	// Cheap and unconditional: TouchTyping is a no-op unless already typing.
	if a.onActivity != nil {
		a.onActivity(a.id)
	}
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
				} else if a.onMessage != nil && !private {
					// A suppressed PASS is INVISIBLE on the board by design -- which once
					// made a healthy agent look broken (it was PASS-ing a repeated request
					// in ~2s while the operator saw pure silence). Log it so "agent never
					// responds" is diagnosable from daemon.log in seconds, not by exhuming
					// the CLI's session transcript.
					log.Printf("[route] %q replied PASS (suppressed from the board)", a.id)
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

// Interrupt cancels the agent's IN-FLIGHT turn WITHOUT killing the process, by
// writing a stream-json control_request{interrupt} to the CLI's stdin -- the same
// mechanism the Claude Agent SDK's interrupt() uses. The process stays alive and
// its session is untouched; only the current generation stops. This is the light
// alternative to a respawn (kill + relaunch): no restart, no session reload. The
// CLI emits its own end-of-turn result afterward, which readLoop handles (so the
// pendingPrivate queue stays matched and typing clears normally).
func (a *Agent) Interrupt() error {
	req := map[string]interface{}{
		"type":       "control_request",
		"request_id": fmt.Sprintf("int_%s_%d", a.id, time.Now().UnixNano()),
		"request":    map[string]interface{}{"subtype": "interrupt"},
	}
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.in == nil {
		return fmt.Errorf("agent %s has no live stdin to interrupt", a.id)
	}
	if _, err := a.in.Write(b); err != nil {
		return err
	}
	if err := a.in.WriteByte('\n'); err != nil {
		return err
	}
	return a.in.Flush()
}

// sendPrompt ENQUEUES a turn for this agent's own sendLoop to write; it does not
// touch stdin itself. The enqueue is non-blocking by construction: a wedged
// recipient (one not draining its stdin) can only fill its bounded sendCh, never
// stall the caller -- which for a board reply is the SENDER's readLoop. A full
// queue (recipient genuinely stuck) is reported so board.Post can log "not woken"
// rather than the whole fan-out hanging. A gone process is likewise a clean error.
func (a *Agent) sendPrompt(text string, private bool) error {
	select {
	case a.sendCh <- sendJob{text: text, private: private}:
		return nil
	case <-a.exited:
		return fmt.Errorf("agent %q is gone", a.id)
	default:
		return fmt.Errorf("agent %q send queue full (%d) -- not draining stdin", a.id, cap(a.sendCh))
	}
}

// sendLoop is the only writer of queued TURNS to this agent's stdin (Interrupt
// also writes to a.in -- a control_request, never a turn -- and both hold a.mu for
// their entire write+flush, which is the actual no-interleaving guarantee; any new
// a.in write MUST take a.mu too). Draining the queue here, off the caller's
// goroutine, is what turns a stuck recipient from a head-of-line stall (froze a
// sender's turn, hung shutdown) into a purely local backlog. Because it exits on
// `exited` (closed when the process is confirmed gone) rather than on a channel
// close, there is no send-on-closed race with a concurrent sendPrompt, and no
// goroutine leak: a Kill tree-kills the process, the in-flight write below errors
// out, and this loop then sees `exited` and returns. One accepted edge: if an
// enqueue and process-death race, a job can land in sendCh just as this loop exits
// -- the turn is undeliverable either way (agent is gone); the only artifact is the
// [route] log optimistically counting that agent as woken.
func (a *Agent) sendLoop() {
	for {
		select {
		case job := <-a.sendCh:
			a.writeTurn(job.text, job.private)
		case <-a.exited:
			return
		}
	}
}

// writeTurn does the actual stdin write for ONE turn. ONLY sendLoop calls it, so the
// pendingPrivate push stays in strict FIFO with the writes (the CLI resolves queued
// turns in that same order). Errors are logged, not returned -- the caller already
// moved on at enqueue; a failed write here just means the process is going away.
func (a *Agent) writeTurn(text string, private bool) {
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
		a.clearTyping() // never actually reached (this struct always marshals) -- kept honest
		log.Printf("[agent %s] marshal turn failed: %s", a.id, err)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// Push BEFORE the write, in the same order the write happens -- the queue
	// order must match the order turns actually reach the CLI's stdin.
	a.pendingPrivate = append(a.pendingPrivate, private)
	if _, err := a.in.Write(b); err != nil {
		a.undoPendingPrivateLocked() // this turn never actually reached the process -- don't leave a phantom queue entry
		a.clearTyping()
		log.Printf("[agent %s] stdin write failed (process gone?): %s", a.id, err)
		return
	}
	if err := a.in.WriteByte('\n'); err != nil {
		a.undoPendingPrivateLocked()
		a.clearTyping()
		log.Printf("[agent %s] stdin write failed: %s", a.id, err)
		return
	}
	if err := a.in.Flush(); err != nil {
		a.undoPendingPrivateLocked()
		a.clearTyping()
		log.Printf("[agent %s] stdin flush failed: %s", a.id, err)
		return
	}
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
// agentKillTimeout bounds how long Kill waits for confirmed exit. Generous
// enough that a process merely slow to die is not misreported, short enough
// that a restart cannot hang the tray or an HTTP handler indefinitely.
const agentKillTimeout = 10 * time.Second

// Kill terminates the subprocess and WAITS for it to actually be gone.
//
// It previously returned as soon as the signal was delivered. RestartAll then
// called Spawn for the same id immediately, so a replacement process could
// start while its predecessor was still tearing down -- two live processes for
// one agent id, with the old one's readLoop still wired to onMessage and so
// still able to post to the board under that id. Returning on confirmed exit
// is what makes "one agent id, one process" true of the OS and not just of the
// registry map.
func (a *Agent) Kill() error {
	a.dying.Store(true) // Spawn refuses to reuse this id until the process is gone
	// No process to wait on -- nothing can ever close exited, so returning here
	// is the only correct answer. (a.cmd is always set by NewAgent; the nil
	// check keeps this honest rather than half-defensive.)
	if a.cmd == nil || a.cmd.Process == nil {
		return nil
	}
	killProcessTree(a.cmd.Process.Pid) // best-effort: the CLI's own children
	_ = a.cmd.Process.Kill()
	select {
	case <-a.exited:
		return nil
	case <-time.After(agentKillTimeout):
		// Report it rather than pretending: an unreaped process that LOOKS
		// reaped is exactly the orphan that produces duplicate output.
		return fmt.Errorf("agent %q did not exit within %s of being killed", a.id, agentKillTimeout)
	}
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

// PreviewCommand returns the exact binary and arguments an agent WOULD be
// launched with, built by the same buildCLICommand the launcher uses. It exists
// so the UI can show the real command instead of describing it: a hand-written
// description and a runtime flag can disagree, and this one did -- the settings
// dialog claimed OFF meant "scoped to its own folder", which was never true.
//
// Derive the wording from this; never maintain a parallel sentence.
func PreviewCommand(opts AgentOptions) (string, []string, error) {
	return buildCLICommand(opts)
}
