package main

import "sync"

// ringBuffer is ClaudeCanvas's documented pattern (docs/electron/terminal-server.md):
// each agent buffers its own output regardless of whether anyone's watching,
// bounded by total byte size (their spec: 256KB per agent), oldest evicted
// first. A viewer that reconnects gets the buffer flushed to it before going
// live, so a disconnect never loses history -- it was never "gone," just
// unwatched.
type ringBuffer struct {
	mu      sync.Mutex
	events  []NormalizedEvent
	sizes   []int
	total   int
	maxSize int
}

const ringBufferMaxBytes = 256 * 1024 // matches ClaudeCanvas's own spec exactly

func newRingBuffer(maxSize int) *ringBuffer {
	return &ringBuffer{maxSize: maxSize}
}

func (r *ringBuffer) Add(e NormalizedEvent, approxSize int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	r.sizes = append(r.sizes, approxSize)
	r.total += approxSize
	for r.total > r.maxSize && len(r.events) > 0 {
		r.total -= r.sizes[0]
		r.events = r.events[1:]
		r.sizes = r.sizes[1:]
	}
}

// Snapshot returns a copy of everything currently buffered, oldest first --
// exactly what a reconnecting viewer needs replayed before going live.
func (r *ringBuffer) Snapshot() []NormalizedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]NormalizedEvent, len(r.events))
	copy(out, r.events)
	return out
}
