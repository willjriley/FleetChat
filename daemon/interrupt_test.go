package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

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

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan struct{})
	go func() {
		runQwenAdapterIO([]string{"--qwen-bin", fakeQwen(t, dir, marker), "--repo", dir}, inR, outW)
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

	start := time.Now()
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
		t.Fatal("no result within 30s of the interrupt -- the turn was NOT cancelled (the no-op regression)")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("turn took %v to end -- consistent with the child finishing on its own, not with being interrupted", elapsed)
	}

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
