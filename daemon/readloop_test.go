package main

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// TestAgentLineBudgetCoversRealisticMessages guards the trigger for the
// 2026-07-31 silent-deafness incident.
//
// An agent posted a message whose single JSON line exceeded the 1MB stdout
// budget. bufio.Scanner returned "token too long", the read loop ended, and the
// agent went on being listed as alive while the board accepted six wakes and got
// nothing back.
//
// The budget is asserted through the SAME constant the production scanners use,
// so lowering it in agent.go fails here rather than in production 50 minutes
// later. Board messages carrying tables, translated documents and model
// inventories are routinely hundreds of KB; 1MB was not a generous ceiling, it
// was inside normal range.
func TestAgentLineBudgetCoversRealisticMessages(t *testing.T) {
	if maxAgentLineBytes < 4*1024*1024 {
		t.Fatalf("maxAgentLineBytes is %d; a long crew message overflows well under 4MB "+
			"and the overflow presents as a silently deaf agent, not an error",
			maxAgentLineBytes)
	}

	// A realistic long message: the kind of table/translation payload that broke it.
	long := strings.Repeat("x", 1_500_000)
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + long + `"}]}}`

	sc := bufio.NewScanner(bytes.NewReader([]byte(line + "\n")))
	sc.Buffer(make([]byte, 0, 64*1024), maxAgentLineBytes)
	if !sc.Scan() {
		t.Fatalf("a %d-byte line did not scan under the production budget: %v", len(line), sc.Err())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan error on a realistic long line: %v", err)
	}

	// And prove the OLD budget genuinely failed on it -- otherwise this test
	// passes for reasons unrelated to the fix.
	old := bufio.NewScanner(bytes.NewReader([]byte(line + "\n")))
	old.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if old.Scan() {
		t.Fatal("the pre-fix 1MB budget accepted this line, so this test proves nothing " +
			"about the regression it exists to catch")
	}
	if old.Err() == nil || !strings.Contains(old.Err().Error(), "token too long") {
		t.Fatalf("expected the old budget to fail with 'token too long', got %v", old.Err())
	}
}

// TestStderrScannerHasExplicitBudget: the stderr reader had NO .Buffer() call at
// all, so it ran on Go's 64KB default -- a far lower ceiling than stdout's, on a
// stream that carries stack traces. Its overflow closes stderrDone early and lets
// readLoop reap a process that is still alive, which is the same failure by a
// different door.
//
// Asserted against the source rather than by running the goroutine: the goroutine
// needs a live child process, and the property under test is that the call exists
// with the shared constant.
func TestStderrScannerHasExplicitBudget(t *testing.T) {
	src := readSourceFile(t, "agent.go")

	i := strings.Index(src, "agent %s stderr")
	if i < 0 {
		t.Fatal("could not locate the stderr reader in agent.go")
	}
	// Look back from the log line to the scanner construction for this reader.
	window := src[max0(i-900):i]
	if !strings.Contains(window, "scanner.Buffer(") {
		t.Fatal("the stderr scanner has no explicit Buffer() -- it will run on Go's " +
			"64KB default, and one long stderr line will end the reader while the " +
			"process is still alive")
	}
	if !strings.Contains(window, "maxAgentLineBytes") {
		t.Fatal("the stderr scanner does not use maxAgentLineBytes -- stdout and stderr " +
			"drifting apart is what let the stdout bug hide in the first place")
	}
}

// TestStreamErrorKillsInsteadOfBlocking is the important one: it pins the actual
// defect rather than its trigger.
//
// A scanner error ends the read loop while the child is STILL ALIVE. The old code
// fell straight through to cmd.Wait(), which blocks forever on a living process --
// so the deferred clearTyping/onExit never ran and the registry kept reporting the
// agent as healthy. Raising the buffer only makes that rarer; it does not fix it.
// The fix is to kill deliberately on a desynchronised stream.
func TestStreamErrorKillsInsteadOfBlocking(t *testing.T) {
	src := readSourceFile(t, "agent.go")

	i := strings.Index(src, "read loop ended with error")
	if i < 0 {
		t.Fatal("could not locate the read-loop termination handling")
	}
	branch := src[i:min(len(src), i+900)]
	if !strings.Contains(branch, "killProcessTree") && !strings.Contains(branch, "Process.Kill") {
		t.Fatal("a scanner error does not kill the child. cmd.Wait() will block on a " +
			"live process, cleanup never runs, and the agent stays listed as alive " +
			"while silently unread -- the 2026-07-31 failure exactly")
	}
}

func max0(i int) int {
	if i < 0 {
		return 0
	}
	return i
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// readSourceFile reads a daemon source file for the source-asserting tests above.
// Those properties (a Buffer() call exists; a stream error kills) are structural:
// exercising them at runtime would need a live child process, and the thing worth
// pinning is that the code says so, not that one execution happened to survive.
func readSourceFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// TestPumpStderrKeepsDrainingAfterOverCapLine guards HIGH-1.
//
// Raising maxAgentLineBytes only MOVES the threshold; it does not remove this
// failure. When a single stderr line exceeds the cap the scan loop ends with
// "token too long", and if the pump returned there, nothing would ever read the
// stderr pipe again. A real child then blocks on its next stderr write once the
// OS buffer fills, therefore stops writing stdout, therefore readLoop's Scan()
// blocks FOREVER -- no EOF, no error, so readDead is never set and sendPrompt
// goes on acking wakes. Same silent-deafness incident, different door.
//
// io.Pipe is unbuffered, so it models the full-buffer child exactly: every Write
// blocks until someone Reads. If the drain is removed, the writer below wedges
// and this test fails on the timeout rather than passing quietly.
func TestPumpStderrKeepsDrainingAfterOverCapLine(t *testing.T) {
	pr, pw := io.Pipe()

	pumped := make(chan struct{})
	go func() { defer close(pumped); pumpStderr("alice", pr) }()

	wrote := make(chan error, 1)
	go func() {
		big := append(bytes.Repeat([]byte("x"), maxAgentLineBytes+4096), '\n')
		if _, err := pw.Write(big); err != nil {
			wrote <- err
			return
		}
		// Everything AFTER the over-cap line is the part that matters: a child
		// keeps logging, and those writes must keep being consumed.
		for i := 0; i < 32; i++ {
			if _, err := pw.Write(bytes.Repeat([]byte("still logging\n"), 2048)); err != nil {
				wrote <- err
				return
			}
		}
		wrote <- pw.Close()
	}()

	select {
	case err := <-wrote:
		if err != nil {
			t.Fatalf("stderr writer failed: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("stderr writer BLOCKED after the over-cap line: pumpStderr stopped reading. " +
			"A real child would stall on its next stderr write, stop writing stdout, and " +
			"readLoop's Scan() would block forever with no EOF and no error -- a silently " +
			"deaf agent that the board keeps reporting as successfully woken.")
	}

	select {
	case <-pumped:
	case <-time.After(5 * time.Second):
		t.Fatal("pumpStderr did not return once the pipe reached EOF -- stderrDone would " +
			"never close and readLoop's reap would hang on it")
	}
}

// TestReadDeadRefusesWakeInsteadOfAcking is the guard readDead never had.
//
// It needs no child process: the whole contract is that sendPrompt consults the
// flag BEFORE queueing. The positive control matters as much as the negative one
// -- a sendPrompt that refused everything would also "pass" a one-sided test.
func TestReadDeadRefusesWakeInsteadOfAcking(t *testing.T) {
	a := &Agent{id: "alice", sendCh: make(chan sendJob, 4), exited: make(chan struct{})}

	// Positive control: a live reader accepts, and the turn really is queued.
	if err := a.sendPrompt("first", false); err != nil {
		t.Fatalf("a healthy agent must accept a wake, got: %v", err)
	}
	if got := len(a.sendCh); got != 1 {
		t.Fatalf("accepted wake should be queued once, queue depth = %d", got)
	}

	a.readDead.Store(true)

	err := a.sendPrompt("second", false)
	if err == nil {
		t.Fatal("sendPrompt ACKED a wake for an agent whose reader is dead. That is the " +
			"silent-deafness bug itself: the board reports the agent woken and the operator " +
			"waits for a reply that can never arrive.")
	}
	if !strings.Contains(err.Error(), "not woken") {
		t.Errorf("refusal must be legible to the board's not-woken path, got: %v", err)
	}
	if got := len(a.sendCh); got != 1 {
		t.Errorf("a refused wake must NOT be queued; queue depth = %d, want 1", got)
	}
}

// TestPanicInRouteStillMarksAgentDeaf guards MED-1, and specifically guards the
// DEFER ORDER -- the fix is where the defer is registered, not that it exists.
//
// Defers run LIFO. Registering the readDead store as the FIRST statement of
// readLoop runs it LAST: behind the recovery defer, which on this exact path calls
// cmd.Wait() on a child nothing killed and blocks forever. The store then never
// executes at all. So this test uses a REAL live helper process -- with a fake or
// an already-dead child, Wait() returns promptly and the broken order would pass.
//
// stderrDone is pre-closed so the recovery defer reaches Wait() instead of parking
// on the stderr handshake first; Wait() is the block we care about.
func TestPanicInRouteStillMarksAgentDeaf(t *testing.T) {
	stderrDone := make(chan struct{})
	close(stderrDone)

	a := &Agent{
		id:         "alice",
		cmd:        startHelper(t), // real, alive, and NOT killed by this path
		exited:     make(chan struct{}),
		stderrDone: stderrDone,
		sendCh:     make(chan sendJob, 4),
	}
	a.onMessage = func(string, string) { panic("boom from a board callback") }

	line := `{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}` + "\n"
	go a.readLoop(strings.NewReader(line))

	deadline := time.After(5 * time.Second)
	for !a.readDead.Load() {
		select {
		case <-deadline:
			t.Fatal("readDead is still FALSE after a panic in route(). The deferred store " +
				"is queued behind the recovery defer, which is blocked in cmd.Wait() on a " +
				"live child, so it never runs. readDead stays false AND exited never closes " +
				"-- both sendPrompt gates dead at once while the board keeps acking wakes.")
		case <-time.After(10 * time.Millisecond):
		}
	}

	if err := a.sendPrompt("wake", false); err == nil {
		t.Fatal("sendPrompt ACKED a wake after a panic killed the read loop")
	}
}
