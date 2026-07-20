package main

import (
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// boardServer makes "the board" -- the HTTP/WS server PLUS the crew of agents --
// a start/stoppable unit decoupled from the tray/process lifetime. This is what
// lets "Shut down board" stop serving and kill the agents while the tray app
// stays alive (with a "Start board" to bring it back), as distinct from "Exit
// application" which quits the whole process. Before this split both tray items
// just os.Exit(0)'d, so there was no board-off-but-app-alive state -- clicking
// "Shut down board" took the entire app (and tray) down.
//
// The side effects are injected (onStart/onStop) rather than hard-wired so the
// lifecycle is unit-testable without spawning real agents: prod wires
// bootstrapFleet / reg.KillAll; a test wires no-ops and a dummy handler.
type boardServer struct {
	mu      sync.Mutex
	running bool
	srv     *http.Server
	ln      net.Listener

	addr    string
	handler http.Handler
	onStart func() // bring the crew up (prod: bootstrapFleet)
	onStop  func() // take the crew down (prod: reg.KillAll)
}

func newBoardServer(addr string, handler http.Handler, onStart, onStop func()) *boardServer {
	return &boardServer{addr: addr, handler: handler, onStart: onStart, onStop: onStop}
}

// Start binds the port (retrying to absorb a restart handoff), serves in a
// goroutine, and brings the crew up. Idempotent: a Start while already running
// is a no-op returning nil. Returns an error only if the port never came free
// within the retry budget -- the caller decides whether that's fatal (initial
// boot log.Fatals) or just a failed toggle (the tray notifies).
func (bs *boardServer) Start() error {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if bs.running {
		return nil
	}
	// Retry the BIND step rather than dying on the first failure: a "Restart
	// board" re-exec launches the new process BEFORE the old frees the port, and
	// Windows can be slow to make it reusable (verified live: 10s was not enough,
	// a real restart ran out of retries and left nothing listening). 60 x 500ms
	// = 30s. Only the bind retries; Serve below runs once on the open listener.
	const bindRetries = 60
	var ln net.Listener
	var err error
	for attempt := 1; attempt <= bindRetries; attempt++ {
		ln, err = net.Listen("tcp", bs.addr)
		if err == nil {
			break
		}
		log.Printf("[board] bind attempt %d/%d failed (%s), retrying...", attempt, bindRetries, err)
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		return err
	}
	bs.ln = ln
	srv := &http.Server{Handler: bs.handler}
	bs.srv = srv
	go func() {
		// Serve returns ErrServerClosed when WE Close it (a clean Stop) -- expected,
		// not an error. Any OTHER return means the listener died under us: do NOT
		// os.Exit (that would kill the tray too, the old fragility) -- just mark the
		// board stopped so the tray's "Start board" can bring it back.
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[board] serve stopped unexpectedly: %s", err)
			bs.markStopped(srv)
		}
	}()
	if bs.onStart != nil {
		bs.onStart()
	}
	bs.running = true
	log.Printf("[board] started -- listening on http://%s", ln.Addr())
	return nil
}

// Stop closes the server and takes the crew down, but leaves the process (and
// tray) alive. Idempotent. State on disk (board.jsonl, threads.json) survives,
// so a later Start resumes the same history with a freshly spawned crew.
func (bs *boardServer) Stop() {
	bs.mu.Lock()
	if !bs.running {
		bs.mu.Unlock()
		return
	}
	srv := bs.srv
	onStop := bs.onStop
	bs.srv, bs.ln, bs.running = nil, nil, false
	bs.mu.Unlock() // release BEFORE Close so the serve goroutine's ErrServerClosed path never contends for the lock
	if srv != nil {
		_ = srv.Close() // immediate close (closes the listener too); WS conns are long-lived so a graceful Shutdown would hang
	}
	if onStop != nil {
		onStop()
	}
	log.Printf("[board] stopped -- tray still running; use 'Start board' to resume")
}

// markStopped handles an UNEXPECTED serve death (a non-ErrServerClosed error --
// the listener died by something other than our own Stop). It flips the board
// off AND tears the crew down (onStop), symmetric with Stop, so we never sit
// "board off but agents still idling with no server to talk to"; a later Start
// re-spawns them. The `bs.srv != srv` guard makes it race harmlessly with an
// explicit Stop: whichever swaps srv out first runs onStop, the other returns
// early -- so onStop fires exactly once.
func (bs *boardServer) markStopped(srv *http.Server) {
	bs.mu.Lock()
	if bs.srv != srv {
		bs.mu.Unlock()
		return
	}
	onStop := bs.onStop
	bs.srv, bs.ln, bs.running = nil, nil, false
	bs.mu.Unlock() // release before onStop (KillAll can be slow), mirroring Stop
	if onStop != nil {
		onStop()
	}
}

// Running reports whether the board is currently serving.
func (bs *boardServer) Running() bool {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	return bs.running
}

// BoundAddr is the actual listen address (useful when addr used port :0), or ""
// when stopped.
func (bs *boardServer) BoundAddr() string {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if bs.ln != nil {
		return bs.ln.Addr().String()
	}
	return ""
}
