package main

// SHIELD review F1 (2026-09-07): a pause must also stop turns ALREADY QUEUED for the
// member. On feat/pause-member @3207c8b this test FAILS (3 of 3 queued turns are
// written to the paused member's stdin) -- it must fail before the fix and pass after.
// Drop it into daemon/ as pause_backlog_test.go once the fix is in.

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestPauseDiscardsTurnsAlreadyQueued(t *testing.T) {
	var buf bytes.Buffer
	a := &Agent{id: "bob", sendCh: make(chan sendJob, 8), exited: make(chan struct{}), in: bufio.NewWriter(&buf)}
	reg := NewRegistry()
	reg.agents["bob"] = a
	for _, txt := range []string{"turn-1", "turn-2", "turn-3"} { // a backlog lands while bob is active...
		if err := a.sendPrompt(txt, false); err != nil {
			t.Fatal(err)
		}
	}
	reg.SetPaused("bob", true) // ...then the operator pauses him before his loop drained it
	go a.sendLoop()
	time.Sleep(300 * time.Millisecond)
	close(a.exited)
	a.mu.Lock()
	out := buf.String()
	a.mu.Unlock()
	if n := strings.Count(out, "turn-"); n != 0 {
		t.Errorf("pause does not stop the backlog: %d of 3 queued turns were still delivered", n)
	}
}
