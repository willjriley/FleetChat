package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// runQwenAdapter is a HELD-OPEN front-end for the one-shot Qwen-Code CLI, so the
// daemon can drive a qwen agent over the SAME stdin/stdout stream-json contract it
// uses for claude -- with ZERO spawn-per-turn logic in the daemon core. The daemon
// launches THIS binary in adapter mode (see qwenCommand) as the agent's "process".
// Each board turn arrives as a stream-json user message on our stdin; we run
// `qwen -p <text> [-r <session>] -o stream-json` in the agent's repo -- qwen's
// stream-json OUTPUT already matches claude's schema, so we forward it verbatim --
// then stay alive for the next turn. The session id is sniffed off qwen's own
// system/init line and reused as -r, so the conversation continues across turns and
// across daemon restarts (the daemon re-passes the saved id via --resume).
//
// protocolRules() (the board operating rules -- addressing, the PASS convention,
// the task card API) reaches qwen OUT OF BAND, as its SYSTEM PROMPT -- the analog
// of claude's --append-system-prompt. qwen-code has no append flag, but it honors
// QWEN_SYSTEM_MD=<file>, which REPLACES its base system prompt with that file and
// is never written into the saved chat transcript. So buildQwenSystemPrompt()
// captures qwen's own base prompt once (via QWEN_WRITE_SYSTEM_MD, cached), stages
// "<base>\n\n---\n\n<rules>" per launch, and we point QWEN_SYSTEM_MD at it on the
// qwen child's env only (a hand-run/resumed qwen in the repo has no such env, so it
// carries NO board rules -- the same no-taint property claude has). This delivers
// the CURRENT rules every launch with ZERO transcript growth (empirically verified:
// a 22KB base+rules prompt leaves 0 bytes in the .jsonl, yet is obeyed).
//
// Fallback: if the base capture fails (e.g. qwen's model endpoint is down at
// launch), we revert to PREPENDING protocolRules() into the first turn -- the old
// in-conversation path -- so the agent is never left rules-blind, at the cost of
// that copy landing in the transcript. firstTurn tracks that fallback only.
// The model is deliberately unset -- qwen uses its own configured default.
func runQwenAdapter(args []string) {
	repo, resume, qwenBin, fullPerms, extra := parseQwenAdapterArgs(args)
	session := resume

	// Preferred path: deliver protocolRules() as qwen's SYSTEM PROMPT via
	// QWEN_SYSTEM_MD (out of band, never in the transcript -- see the func comment).
	// sysPromptFile is the staged "<base>\n\n---\n\n<rules>" file; usePrepend is the
	// fallback (base capture failed -> put the rules in the first turn instead).
	sysPromptFile, sysErr := buildQwenSystemPrompt(qwenBin)
	usePrepend := sysErr != nil
	if usePrepend {
		fmt.Fprintf(os.Stderr, "[qwen-adapter] system-prompt delivery unavailable (%v); falling back to in-conversation rules\n", sysErr)
	}
	// firstTurn drives the FALLBACK only: prepend the rules to the first turn that
	// runs to a result. With the system-prompt path working, no prepend is needed.
	firstTurn := usePrepend

	out := bufio.NewWriter(os.Stdout)
	emit := func(line string) {
		out.WriteString(line)
		out.WriteByte('\n')
		out.Flush()
	}
	// A synthetic end-of-turn result so the daemon's turn accounting (the
	// pendingPrivate FIFO + typing indicator) stays matched even when qwen dies
	// without emitting its own result.
	emitResult := func(msg string) {
		b, _ := json.Marshal(map[string]interface{}{
			"type": "result", "subtype": "error", "is_error": true,
			"session_id": session, "result": msg,
		})
		emit(string(b))
	}

	var mu sync.Mutex
	var cur *exec.Cmd

	// A DEDICATED stdin reader, so an interrupt lands DURING a turn: the run loop
	// below blocks forwarding qwen's output and can't read stdin itself. The reader
	// kills the in-flight qwen on control_request{interrupt} (mirroring claude's
	// mid-turn Stop) and queues user turns for the run loop. session and firstTurn
	// stay owned solely by the run loop below, so the split can't race.
	//
	// Turns go into an UNBOUNDED cond-var queue, not a fixed channel: the reader
	// must NEVER block handing off a turn, or a control_request queued behind a
	// backlog of user turns would go unread and the in-flight qwen would never be
	// killed -- and Stop is the kill switch for a --approval-mode yolo agent. A
	// bounded channel could fill (the daemon's own send queue is 64 deep) and wedge
	// the reader on the send; an append-and-signal reader stays free to see the next
	// interrupt regardless of backlog. The daemon's send queue still bounds real
	// growth, so "unbounded" here can't actually run away.
	var (
		qmu      sync.Mutex
		qcond    = sync.NewCond(&qmu)
		queue    []string
		inClosed bool
	)
	go func() {
		in := bufio.NewScanner(os.Stdin)
		in.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for in.Scan() {
			line := strings.TrimSpace(in.Text())
			if line == "" {
				continue
			}
			var m struct {
				Type    string `json:"type"`
				Request struct {
					Subtype string `json:"subtype"`
				} `json:"request"`
				Message *struct {
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal([]byte(line), &m) != nil {
				continue
			}
			if m.Type == "control_request" && m.Request.Subtype == "interrupt" {
				mu.Lock()
				if cur != nil && cur.Process != nil {
					// Kill the in-flight turn; its qs.Scan() then ends and the synthetic
					// no-result below keeps the daemon's turn accounting matched.
					_ = cur.Process.Kill()
				}
				mu.Unlock()
				continue
			}
			if m.Type != "user" || m.Message == nil {
				continue
			}
			var sb strings.Builder
			for _, c := range m.Message.Content {
				if c.Type == "text" {
					sb.WriteString(c.Text)
				}
			}
			qmu.Lock()
			queue = append(queue, sb.String())
			qmu.Unlock()
			qcond.Signal() // never blocks the reader -> interrupts always get read
		}
		qmu.Lock()
		inClosed = true
		qmu.Unlock()
		qcond.Broadcast() // stdin closed -> wake the run loop to drain + return
	}()

	// nextTurn blocks until a turn is available (text, true) or stdin has closed
	// AND the queue is drained ("", false). FIFO, single consumer (the run loop).
	nextTurn := func() (string, bool) {
		qmu.Lock()
		defer qmu.Unlock()
		for len(queue) == 0 && !inClosed {
			qcond.Wait()
		}
		if len(queue) == 0 {
			return "", false
		}
		t := queue[0]
		queue = queue[1:]
		return t, true
	}

	for {
		text, ok := nextTurn()
		if !ok {
			break
		}
		prompt := text
		if firstTurn {
			// Prepend the rulebook, but do NOT consume firstTurn here: it is cleared
			// only after the turn actually produces a result (see below). A first turn
			// that fails to start or is interrupted then re-teaches on the next turn,
			// instead of leaving the whole launch rules-blind.
			prompt = protocolRules() + "\n\n---\n\n" + prompt
		}
		// Prompt goes on STDIN, not as a -p arg: qwen reads stdin as the prompt in
		// non-interactive mode, and a long/multi-line -p value passed through qwen.cmd
		// (the Windows batch shim) mangles newlines and drops later flags. Verified:
		// stdin + -o stream-json yields clean stream-json.
		qargs := []string{"-o", "stream-json"}
		// Validate the session id at point-of-USE (covers BOTH the hand-launched
		// --resume arg and the id sniffed off qwen's own stream); a malformed one is
		// dropped so the turn starts a FRESH qwen session rather than aborting.
		if session != "" && validSessionID.MatchString(session) {
			qargs = append(qargs, "-r", session)
		}
		if fullPerms {
			qargs = append(qargs, "--approval-mode", "yolo") // full permissions: qwen's max approval bypass
		}
		qargs = appendExtra(qargs, extra) // last, so a last-wins flag can override ours
		qc := exec.Command(qwenBin, qargs...)
		if repo != "" {
			qc.Dir = repo
		}
		qc.Stdin = strings.NewReader(prompt)
		qc.Stderr = os.Stderr
		// Deliver the board rules as qwen's system prompt (out of band) on THIS child
		// only. Scoped to the env we pass, so a qwen the operator runs by hand in the
		// same repo never inherits it -- claude's no-taint property, matched.
		if !usePrepend {
			qc.Env = append(os.Environ(), "QWEN_SYSTEM_MD="+sysPromptFile)
		}
		stdout, err := qc.StdoutPipe()
		if err == nil {
			err = qc.Start()
		}
		if err != nil {
			emitResult("qwen failed to start: " + err.Error())
			continue
		}
		mu.Lock()
		cur = qc
		mu.Unlock()

		sawResult := false
		qs := bufio.NewScanner(stdout)
		qs.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for qs.Scan() {
			l := qs.Text()
			emit(l) // forward verbatim -- qwen's schema already matches claude's
			var raw struct {
				Type      string `json:"type"`
				SessionID string `json:"session_id"`
			}
			if json.Unmarshal([]byte(l), &raw) == nil {
				if raw.Type == "result" {
					sawResult = true
				}
				if session == "" && raw.SessionID != "" {
					session = raw.SessionID // capture qwen's session id to resume next turn
				}
			}
		}
		_ = qc.Wait()
		mu.Lock()
		cur = nil
		mu.Unlock()
		if !sawResult {
			emitResult("qwen turn ended with no result")
		}
		if sawResult {
			// Delivery confirmed: this turn ran to a result, so its prompt (which
			// carried the rules when firstTurn was set) reached qwen. Only now retire
			// firstTurn. A failed/interrupted turn leaves it set, so the rules ride
			// the next turn -- the fix for a launch that would otherwise run rules-blind.
			firstTurn = false
		}
	}
}

// buildQwenSystemPrompt stages the file that QWEN_SYSTEM_MD points at:
// "<qwen's own base system prompt>\n\n---\n\n<protocolRules()>". QWEN_SYSTEM_MD
// REPLACES qwen's base prompt, so we must include the base or the agent loses
// qwen's coding mandates + tool guidance. The base is captured ONCE (cached in the
// data dir) by running qwen with QWEN_WRITE_SYSTEM_MD, which makes it write its
// resolved base prompt to disk; the combined file is (re)written every call so the
// rules stay current. Returns the staged file's path, or an error if the base can't
// be captured (e.g. qwen's model endpoint is down) so the caller can fall back to
// the in-conversation prepend rather than run the agent rules-blind.
//
// NOTE: after a qwen upgrade the cached base can go stale -- delete
// <data>/qwen-sysprompt/qwen-base.md to force a fresh capture.
func buildQwenSystemPrompt(qwenBin string) (string, error) {
	dir := os.Getenv("FLEETCHAT_DATA_DIR")
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "fleetchat")
	}
	dir = filepath.Join(dir, "qwen-sysprompt")
	// 0700, not 0755: this dir holds the file that BECOMES the agent's system
	// prompt. Under the shared-/tmp fallback, a world-searchable dir would let
	// another local user pre-create qwen-base.md and thereby set the prompt. Owner
	// only shuts that down. (The normal path is under the repo's own data dir.)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}

	baseFile := filepath.Join(dir, "qwen-base.md")
	if _, err := os.Stat(baseFile); err != nil {
		// Capture qwen's resolved base prompt to a temp file, then rename in (atomic,
		// so two adapters capturing at once can't read a half-written base). qwen
		// writes the base while CONSTRUCTING the system prompt for the throwaway turn.
		tmp := baseFile + ".tmp"
		capCmd := exec.Command(qwenBin, "-p", "hi", "-o", "stream-json")
		capCmd.Dir = dir // keep the throwaway capture's transcript out of the agent's repo
		capCmd.Env = append(os.Environ(), "QWEN_WRITE_SYSTEM_MD="+tmp)
		capCmd.Stdout = io.Discard
		capCmd.Stderr = io.Discard
		_ = capCmd.Run() // best-effort; success is judged by whether tmp got written
		if fi, statErr := os.Stat(tmp); statErr != nil || fi.Size() == 0 {
			_ = os.Remove(tmp)
			return "", fmt.Errorf("qwen base-prompt capture wrote no file (model endpoint down?)")
		}
		if err := os.Rename(tmp, baseFile); err != nil {
			return "", fmt.Errorf("stage base prompt: %w", err)
		}
	}

	base, err := os.ReadFile(baseFile)
	if err != nil {
		return "", fmt.Errorf("read cached base prompt: %w", err)
	}
	combined := string(base) + "\n\n---\n\n" + protocolRules()

	// One combined file PER agent (id is validated [a-z0-9_-], filesystem-safe), so
	// concurrent adapters never clobber each other; written via temp+rename so qwen
	// can't read a torn file. Overwritten each launch -> bounded, always current.
	agent := os.Getenv("FLEETCHAT_AGENT")
	if agent == "" {
		agent = "default"
	}
	outFile := filepath.Join(dir, "sysprompt-"+agent+".md")
	tmp := outFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(combined), 0o644); err != nil {
		return "", fmt.Errorf("write combined system prompt: %w", err)
	}
	if err := os.Rename(tmp, outFile); err != nil {
		return "", fmt.Errorf("stage combined system prompt: %w", err)
	}
	return outFile, nil
}

// parseQwenAdapterArgs reads the argv qwenCommand built for this adapter.
//
// Split out of runQwenAdapter purely to be testable: the daemon builds this
// argv and the adapter consumes it, and the two halves living in different
// processes is exactly the seam where an argument can vanish without anything
// failing. A round-trip test over this function is what proves the operator's
// flags actually arrive.
func parseQwenAdapterArgs(args []string) (repo, resume, qwenBin string, fullPerms bool, extra []string) {
	qwenBin = "qwen"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--repo":
			if i+1 < len(args) {
				repo = args[i+1]
				i++
			}
		case "--resume":
			if i+1 < len(args) {
				resume = args[i+1]
				i++
			}
		case "--qwen-bin":
			if i+1 < len(args) {
				qwenBin = args[i+1]
				i++
			}
		case "--full-perms":
			fullPerms = true // -> qwen --approval-mode yolo (max bypass)
		case "--extra-arg":
			// Repeated once per argument, so each one crosses the process
			// boundary as a discrete argv element and is never re-split here.
			if i+1 < len(args) {
				extra = append(extra, args[i+1])
				i++
			}
		}
	}
	return repo, resume, qwenBin, fullPerms, extra
}
