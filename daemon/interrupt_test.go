package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// fakeQwenRuntime is how long the fake child blocks. It is the discriminator for
// every timing assertion here: the turn must end far sooner than this, so "it
// ended" cannot be the child finishing on its own. Named rather than repeated as
// a literal so the assertions can quote it when they fail.
const fakeQwenRuntime = 60 * time.Second

// fakeQwen writes a stand-in for the qwen binary that ignores every argument,
// records each time it is started, and then blocks for a long time.
//
// Ignoring arguments matters: the adapter builds qwen's argv itself (-o
// stream-json and friends), so a real command like `ping` would reject those
// flags and exit immediately -- which would make an interrupt test pass without
// ever interrupting anything.
//
// The marker file is how the test knows a child is genuinely RUNNING. Polling
// for that beats adding an exported hook to the adapter purely so a test can
// watch it: the production code stays as it ships.
//
// The 60s block is the test's whole discriminator -- long enough that "the turn
// ended promptly" cannot be the child finishing on its own.
func fakeQwen(t *testing.T, dir, marker string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		p := filepath.Join(dir, "fakeqwen.bat")
		body := "@echo off\r\n" +
			"echo start>>\"" + marker + "\"\r\n" +
			"ping -n 61 127.0.0.1 >nul\r\n"
		if err := os.WriteFile(p, []byte(body), 0o700); err != nil {
			t.Fatalf("write fake qwen: %v", err)
		}
		return p
	}
	p := filepath.Join(dir, "fakeqwen.sh")
	body := "#!/bin/sh\necho start >> '" + marker + "'\nsleep 60\n"
	if err := os.WriteFile(p, []byte(body), 0o700); err != nil {
		t.Fatalf("write fake qwen: %v", err)
	}
	return p
}

// starts reports how many times the fake qwen has been launched, counted from
// the lines it appends. A missing file is zero, not an error: the test polls
// this before the first launch has happened.
func starts(marker string) int {
	b, err := os.ReadFile(marker)
	if err != nil {
		return 0
	}
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}

// TestStartAndPublishHoldsLockAcrossStart guards RACE 1 -- the defect where the
// live command was published AFTER the child had already been started, so an
// interrupt arriving in between took the lock, found nothing published, did
// nothing, and let the turn run to completion as though Stop was never pressed.
//
// It exists because the end-to-end interrupt test CANNOT catch that. Verified by
// mutation test rather than assumed: with the ordering reverted, the black-box
// test passes 10/10 under -race. The window is parent-side and a few instructions
// wide, so no stimulus fed through the adapter's stdin can be timed into it.
//
// So this asserts the property directly and deterministically instead: while the
// start is running, the lock must already be held. An interrupt is exactly a
// contender for that lock, so "held across the start" is precisely what makes an
// interrupt arriving mid-launch block and then find a live process rather than a
// nil one. TryLock is the observation -- it must FAIL. With start moved back out
// of the locked region it succeeds and this test fails, which is the guard the
// fix was missing.
func TestStartAndPublishHoldsLockAcrossStart(t *testing.T) {
	var mu sync.Mutex
	var cur *exec.Cmd
	qc := exec.Command("does-not-need-to-exist")

	lockWasFree := false
	err := startAndPublish(&mu, &cur, qc, func() error {
		// Stands in for the fork/exec. A separate goroutine is required: TryLock on
		// the goroutine that already holds it is not the question being asked.
		probe := make(chan bool, 1)
		go func() {
			if mu.TryLock() {
				mu.Unlock()
				probe <- true
				return
			}
			probe <- false
		}()
		lockWasFree = <-probe
		return nil
	})
	if err != nil {
		t.Fatalf("startAndPublish: %v", err)
	}
	if lockWasFree {
		t.Fatal("the lock was FREE while the child was being started -- an interrupt " +
			"arriving mid-launch would see an unpublished command and silently do nothing (RACE 1)")
	}
	if cur != qc {
		t.Fatalf("a successful launch must be published as the live turn: cur=%v want=%v", cur, qc)
	}
}

// TestStartAndPublishUnpublishesFailedLaunch pins the other half: a launch that
// fails must leave NOTHING published, or the interrupt path can later hand
// killProcessTree a pid belonging to a process that never started.
func TestStartAndPublishUnpublishesFailedLaunch(t *testing.T) {
	var mu sync.Mutex
	var cur *exec.Cmd
	qc := exec.Command("does-not-need-to-exist")

	want := fmt.Errorf("launch refused")
	err := startAndPublish(&mu, &cur, qc, func() error { return want })
	if err == nil {
		t.Fatal("a failed launch must report its error")
	}
	if cur != nil {
		t.Fatal("a failed launch was left published as the live turn")
	}
}

// TestUnpublishThenWaitRetiresBeforeReaping guards MED-1 (security review, 2026-07-29):
// the live command must be retired BEFORE Wait reaps it, so an interrupt can
// never hand killProcessTree a pid the kernel has already released -- a raw-int
// process-GROUP kill against a pid Windows may have recycled onto something else.
//
// Deterministic for the same reason as the Race 1 guard: the real window is a
// few instructions wide and unreachable through the adapter's stdin, so this
// observes the ordering directly rather than trying to time a stimulus into it.
func TestUnpublishThenWaitRetiresBeforeReaping(t *testing.T) {
	var mu sync.Mutex
	cur := exec.Command("does-not-need-to-exist")

	publishedAtReap := true
	err := unpublishThenWait(&mu, &cur, func() error {
		// Stands in for Wait. By the time the kernel could release this pid, the
		// interrupt path must already be unable to see it.
		mu.Lock()
		publishedAtReap = cur != nil
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatalf("unpublishThenWait: %v", err)
	}
	if publishedAtReap {
		t.Fatal("the command was STILL published while being reaped -- an interrupt in that " +
			"window would tree-kill a released pid, which Windows may have recycled (MED-1)")
	}
	if cur != nil {
		t.Fatal("the command must be retired once the turn is over")
	}
}

// TestQwenAdapterInterruptCancelsInFlightTurn is the test t68 required and the
// interrupt fix shipped without.
//
// The property: a control_request{interrupt} arriving while a turn is IN FLIGHT
// cancels that turn rather than queueing behind it. The bug it guards was a
// no-op -- the adapter sat scanning the child's output and never read stdin, so
// the interrupt waited in the pipe until the turn ended by itself and then
// killed a process that was already gone.
//
// That bug is invisible to a test that only checks a return value, because the
// broken version behaves identically except in TIMING: the turn still ends,
// just when it was always going to. So the assertion is temporal -- the turn
// must end far sooner than the child's own 60s runtime. A regression to the
// no-op fails here by exceeding the deadline, which is the honest shape for
// this defect.
func TestQwenAdapterInterruptCancelsInFlightTurn(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "qwen-starts.txt")

	// Pre-seed the cached base prompt so adapter startup does not try to capture
	// one by RUNNING the fake -- that capture would block for 60s before the test
	// began, and the failure would read as a broken interrupt.
	sysDir := filepath.Join(dir, "qwen-sysprompt")
	if err := os.MkdirAll(sysDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sysDir, "qwen-base.md"), []byte("base prompt\n"), 0o600); err != nil {
		t.Fatalf("seed base prompt: %v", err)
	}
	t.Setenv("FLEETCHAT_DATA_DIR", dir)

	// Built HERE, not inside the goroutine below: fakeQwen reports a write failure
	// with t.Fatalf, and FailNow from a non-test goroutine is undefined -- the test
	// would not stop, it would run on and misreport as a timeout. vet's
	// testinggoroutine check does not see through the helper call.
	qwenBin := fakeQwen(t, dir, marker)

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan struct{})
	go func() {
		runQwenAdapterIO([]string{"--qwen-bin", qwenBin, "--repo", dir}, inR, outW)
		outW.Close()
		close(done)
	}()

	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	type ev struct {
		Type string `json:"type"`
	}
	// One reader goroutine for the whole test: starting a fresh one per wait
	// would race two scanners over the same stream and drop events.
	events := make(chan ev, 64)
	go func() {
		defer close(events)
		for sc.Scan() {
			var e ev
			if json.Unmarshal(sc.Bytes(), &e) == nil && e.Type != "" {
				events <- e
			}
		}
	}()

	sendTurn := func(text string) {
		t.Helper()
		if _, err := fmt.Fprintf(inW, `{"type":"user","message":{"content":[{"type":"text","text":%q}]}}`+"\n", text); err != nil {
			t.Fatalf("write turn: %v", err)
		}
	}
	waitStarts := func(want int, why string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for starts(marker) < want {
			if time.Now().After(deadline) {
				t.Fatalf("fake qwen start #%d never happened -- %s", want, why)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// --- turn 1: let it genuinely start, THEN interrupt it ----------------
	sendTurn("first")
	// Interrupting before the child exists would exercise the queued-interrupt
	// path instead, and would pass against the very bug this guards.
	waitStarts(1, "the test never reached the in-flight state it exists to test")

	if _, err := io.WriteString(inW, `{"type":"control_request","request":{"subtype":"interrupt"}}`+"\n"); err != nil {
		t.Fatalf("write interrupt: %v", err)
	}

	select {
	case e, ok := <-events:
		if !ok {
			t.Fatal("adapter stream closed instead of ending the turn")
		}
		if e.Type != "result" {
			t.Fatalf("want a result event ending the turn, got %q", e.Type)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("no result within 30s of the interrupt (child runs %v) -- the turn was NOT cancelled (the no-op regression)", fakeQwenRuntime)
	}
	// No second elapsed check here: reaching this point already means the select
	// won its 30s race, so any further "did it take too long" assertion is dead by
	// construction. The deadline above IS the timing assertion.

	// --- the other half: the agent is still usable afterwards -------------
	// An interrupt that cancels the turn by taking the adapter down with it
	// would satisfy every assertion above and be useless in practice.
	sendTurn("second")
	waitStarts(2, "the interrupt left the adapter dead or deaf to further turns")

	// Cancel turn 2 as well before tearing down. Closing stdin on a live turn is
	// legitimate but makes the adapter wait out the child's full 60s, which would
	// turn a fast test into a slow one -- and interrupting twice also shows the
	// stdin reader is still working after the first interrupt, not one-shot.
	if _, err := io.WriteString(inW, `{"type":"control_request","request":{"subtype":"interrupt"}}`+"\n"); err != nil {
		t.Fatalf("write second interrupt: %v", err)
	}
	select {
	case <-events:
	case <-time.After(30 * time.Second):
		t.Fatal("second interrupt did not end its turn -- the stdin reader stopped working after the first")
	}

	inW.Close()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("adapter did not exit after stdin closed")
	}
}
