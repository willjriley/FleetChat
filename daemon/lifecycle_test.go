package main

import (
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestBoardServerLifecycle proves the core of the tray split: the board can be
// started (serving), stopped (port refuses connections + crew taken down) WITHOUT
// tearing down the process, and started AGAIN -- with Start/Stop both idempotent.
// Uses port :0 (a random free port) and injected no-op side effects so it needs
// no real agents.
func TestBoardServerLifecycle(t *testing.T) {
	starts, stops := 0, 0
	h := http.NewServeMux()
	h.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	bs := newBoardServer("127.0.0.1:0", h, func() { starts++ }, func() { stops++ })

	// Start -> serving.
	if err := bs.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !bs.Running() {
		t.Fatal("should be running after Start")
	}
	addr := bs.BoundAddr()
	if addr == "" {
		t.Fatal("no bound addr after Start")
	}
	if got := httpGet(t, "http://"+addr+"/ping"); got != "ok" {
		t.Fatalf("serving: got %q, want ok", got)
	}
	if starts != 1 {
		t.Fatalf("onStart calls = %d, want 1", starts)
	}

	// Idempotent Start -- no second crew bring-up.
	if err := bs.Start(); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if starts != 1 {
		t.Fatalf("idempotent Start re-ran onStart (%d)", starts)
	}

	// Stop -> port refuses, crew down, but this returns (process alive).
	bs.Stop()
	if bs.Running() {
		t.Fatal("should not be running after Stop")
	}
	if stops != 1 {
		t.Fatalf("onStop calls = %d, want 1", stops)
	}
	waitUnreachable(t, addr)

	// Idempotent Stop.
	bs.Stop()
	if stops != 1 {
		t.Fatalf("idempotent Stop re-ran onStop (%d)", stops)
	}

	// Start again -> serving again (proves the listener freed and re-serve works).
	if err := bs.Start(); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if got := httpGet(t, "http://"+bs.BoundAddr()+"/ping"); got != "ok" {
		t.Fatalf("serving after restart: got %q, want ok", got)
	}
	if starts != 2 {
		t.Fatalf("onStart after restart = %d, want 2", starts)
	}
	bs.Stop()
}

// TestMarkStoppedTearsDownCrew covers an UNEXPECTED serve death: markStopped
// must flip the board off AND tear the crew down (onStop), and its srv guard
// must make that happen exactly once even if called again with a stale server.
func TestMarkStoppedTearsDownCrew(t *testing.T) {
	stops := 0
	bs := newBoardServer("127.0.0.1:0", http.NewServeMux(), func() {}, func() { stops++ })
	if err := bs.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	srv := bs.srv     // in-package field access
	defer srv.Close() // real cleanup: markStopped only drops the reference, it doesn't close the server

	bs.markStopped(srv) // simulate the serve goroutine's unexpected-death path
	if bs.Running() {
		t.Fatal("markStopped should mark the board not-running")
	}
	if stops != 1 {
		t.Fatalf("markStopped should tear the crew down once (onStop); stops=%d", stops)
	}
	bs.markStopped(srv) // stale srv -- the guard must skip, no second onStop
	if stops != 1 {
		t.Fatalf("stale markStopped re-ran onStop; stops=%d", stops)
	}
}

func httpGet(t *testing.T, url string) string {
	t.Helper()
	var lastErr error
	for i := 0; i < 40; i++ { // brief poll: the Serve goroutine may not be accepting the instant Start returns
		resp, err := http.Get(url)
		if err != nil {
			lastErr = err
			time.Sleep(25 * time.Millisecond)
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(b)
	}
	t.Fatalf("GET %s never succeeded: %v", url, lastErr)
	return ""
}

func waitUnreachable(t *testing.T, addr string) {
	t.Helper()
	for i := 0; i < 40; i++ {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			return // refused == stopped, good
		}
		c.Close()
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("port %s still reachable after Stop -- server did not close", addr)
}
