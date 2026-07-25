package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// newQueueAgent builds an Agent wired to one end of an io.Pipe standing in for
// the CLI's stdin, plus a running sendLoop -- the minimum real machinery of the
// send path. io.Pipe is UNBUFFERED: a flush blocks until the "process" reads,
// which is exactly how a wedged recipient behaves (its OS pipe full, not
// draining). Returns the read end (the fake process's stdin) and a channel that
// closes when sendLoop exits.
func newQueueAgent(id string) (*Agent, *io.PipeReader, chan struct{}) {
	pr, pw := io.Pipe()
	a := &Agent{
		id:         id,
		in:         bufio.NewWriter(pw),
		subs:       map[*Viewer]bool{},
		buf:        newRingBuffer(1024),
		exited:     make(chan struct{}),
		stderrDone: make(chan struct{}),
		sendCh:     make(chan sendJob, 64),
	}
	loopDone := make(chan struct{})
	go func() { a.sendLoop(); close(loopDone) }()
	return a, pr, loopDone
}

// THE property the send queue exists for: a recipient that never drains its
// stdin must not block the CALLER of sendPrompt (pre-change, that caller was the
// SENDER's readLoop -- the head-of-line stall that froze a live board). Every
// call must return promptly: accepted while the queue has room, a queue-full
// error after, NEVER a hang.
func TestSendPromptNeverBlocksOnAWedgedRecipient(t *testing.T) {
	a, pr, loopDone := newQueueAgent("wedged")
	// No reads from pr: the first writeTurn blocks in Flush, wedged forever.

	const calls = 100 // > 1 in-flight + 64 queued, so the tail must hit queue-full
	done := make(chan struct{})
	var accepted, full int
	go func() {
		defer close(done)
		for i := 0; i < calls; i++ {
			if err := a.sendPrompt(fmt.Sprintf("turn %d", i), false); err == nil {
				accepted++
			} else if strings.Contains(err.Error(), "queue full") {
				full++
			} else {
				t.Errorf("unexpected sendPrompt error: %v", err)
			}
		}
	}()
	select {
	case <-done:
		// all calls returned while the recipient was fully wedged -- the guarantee
	case <-time.After(5 * time.Second):
		t.Fatal("sendPrompt blocked on a wedged recipient -- the head-of-line stall is back")
	}
	if accepted == 0 || full == 0 {
		t.Fatalf("expected both accepted and queue-full outcomes, got accepted=%d full=%d", accepted, full)
	}

	// Unwedge the way Kill does: the process goes away, its pipe closes, the
	// blocked flush errors out, and sendLoop must then exit via `exited`.
	close(a.exited)
	_ = pr.Close()
	select {
	case <-loopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("sendLoop leaked after process death -- it must exit once `exited` closes")
	}
}

// Turn order on the wire must match enqueue order, and the pendingPrivate FIFO
// (which route() uses to tag each turn's reply as board vs private) must match
// that same order -- the property that keeps replies from being mis-tagged.
func TestSendQueuePreservesFIFOAndPrivateFlags(t *testing.T) {
	a, pr, loopDone := newQueueAgent("fifo")

	// A real reader on the far end (the healthy-process case).
	type wire struct {
		Message struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	var mu sync.Mutex
	var got []string
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			var w wire
			if json.Unmarshal(sc.Bytes(), &w) == nil && len(w.Message.Content) > 0 {
				mu.Lock()
				got = append(got, w.Message.Content[0].Text)
				mu.Unlock()
			}
		}
	}()

	wantTexts := []string{"t0", "t1", "t2", "t3", "t4", "t5"}
	wantPrivate := []bool{false, true, false, false, true, false}
	for i, txt := range wantTexts {
		if err := a.sendPrompt(txt, wantPrivate[i]); err != nil {
			t.Fatalf("sendPrompt(%q): %v", txt, err)
		}
	}

	// Wait for all six to arrive on the wire.
	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == len(wantTexts) {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("only %d/%d turns arrived", n, len(wantTexts))
		case <-time.After(10 * time.Millisecond):
		}
	}
	mu.Lock()
	for i, w := range wantTexts {
		if got[i] != w {
			t.Fatalf("wire order broken: got[%d]=%q want %q (full: %v)", i, got[i], w, got)
		}
	}
	mu.Unlock()

	// pendingPrivate must mirror the same FIFO (no results popped it yet).
	a.mu.Lock()
	if len(a.pendingPrivate) != len(wantPrivate) {
		t.Fatalf("pendingPrivate len=%d want %d", len(a.pendingPrivate), len(wantPrivate))
	}
	for i, w := range wantPrivate {
		if a.pendingPrivate[i] != w {
			t.Fatalf("pendingPrivate[%d]=%v want %v", i, a.pendingPrivate[i], w)
		}
	}
	a.mu.Unlock()

	close(a.exited)
	_ = pr.Close()
	select {
	case <-loopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("sendLoop did not exit")
	}
}

// An idle sendLoop (nothing queued) must exit as soon as the process is
// confirmed gone -- otherwise every agent restart leaks a goroutine.
func TestSendLoopExitsWhenIdleOnExited(t *testing.T) {
	a, pr, loopDone := newQueueAgent("idle")
	close(a.exited)
	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("idle sendLoop did not exit on `exited`")
	}
	_ = pr.Close()
	// A send AFTER death must return promptly. Its select has both arms ready
	// (room in sendCh, exited closed), so the OUTCOME is deliberately unasserted
	// -- Go picks randomly, and the enqueue-wins case is the documented, benign
	// dropped-turn edge (agent is gone either way). The guarantee is no hang.
	done := make(chan struct{})
	go func() { _ = a.sendPrompt("late", false); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sendPrompt hung after agent death")
	}
}