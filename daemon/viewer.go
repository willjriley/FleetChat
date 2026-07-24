package main

import (
	"context"
	"log"
	"sync"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Viewer wraps one connected browser. Sends go through a buffered channel +
// dedicated goroutine so a slow/stuck viewer can never block the agent's
// broadcast loop -- a full channel just drops that viewer's oldest events
// rather than stalling everyone else.
type Viewer struct {
	conn *websocket.Conn
	ctx  context.Context
	out  chan NormalizedEvent

	mu     sync.Mutex
	closed bool
}

func NewViewer(ctx context.Context, conn *websocket.Conn) *Viewer {
	v := &Viewer{conn: conn, ctx: ctx, out: make(chan NormalizedEvent, 64)}
	go v.writeLoop()
	return v
}

// Send and Close share ONE mutex specifically to close a real race:
// Agent.broadcast() snapshots its viewer list under ITS OWN lock, releases
// it, then calls Send() on each viewer outside that lock. If a viewer
// disconnects (Unsubscribe + Close) in the window between the snapshot and
// this Send(), the old code could write to an already-closed channel --
// "send on closed channel" panics, not a theoretical bug. Serializing both
// under v.mu makes the two orderings both safe: either Send() wins the race
// and delivers normally, or Close() wins and Send() sees closed=true and
// quietly no-ops instead of touching the channel at all.
func (v *Viewer) Send(e NormalizedEvent) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return
	}
	select {
	case v.out <- e:
	default:
		log.Printf("[viewer] output channel full, dropping an event (viewer too slow)")
	}
}

func (v *Viewer) writeLoop() {
	for e := range v.out {
		if err := wsjson.Write(v.ctx, v.conn, e); err != nil {
			log.Printf("[viewer] write failed, closing: %s", err)
			return
		}
	}
}

func (v *Viewer) Close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return
	}
	v.closed = true
	close(v.out)
}
