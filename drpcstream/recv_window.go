// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import "sync"

// recvWindow is the per-stream receive side of flow control. It tracks how many
// bytes are currently buffered for the stream (in-progress reassembly plus
// completed, not-yet-consumed data) and decides when to return credit to the
// sender.
//
// The receiver holds no credit balance of its own; it accrues "returnable"
// credit as bytes are dispatched off the wire and releases it as grants:
//   - while buffered is at or above the high-water mark, grants are withheld
//     (backpressure), though the returnable credit keeps accruing;
//   - otherwise, accrued credit is released once it reaches the threshold,
//     coalescing many frames into a single grant.
//
// dispatched and consumed each return the credit delta the caller should send
// as a KindWindowUpdate (0 when nothing should be sent). Emitting the frame is
// the caller's job. dispatched (reader goroutine) and consumed (application
// goroutine) can run concurrently, so the state is mutex-guarded.
type recvWindow struct {
	mu        sync.Mutex
	buffered  int64 // in-progress reassembly + completed, not-yet-consumed bytes
	pending   int64 // returnable credit accrued but not yet granted
	highWater int64 // withhold grants while buffered is at or above this
	threshold int64 // release accrued credit once it reaches this
}

// newRecvWindow returns a recvWindow with the given high-water mark and grant
// threshold.
func newRecvWindow(highWater, threshold int64) *recvWindow {
	return &recvWindow{highWater: highWater, threshold: threshold}
}

// dispatched records that n bytes were dispatched off the wire into the
// stream's buffers and returns the credit delta to grant (0 if none).
func (w *recvWindow) dispatched(n int64) int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buffered += n
	w.pending += n
	return w.maybeGrantLocked()
}

// consumed records that n bytes were consumed by the application and returns
// the credit delta to grant (0 if none). Consuming can reopen a gate that was
// closed by the high-water mark, flushing credit accrued during the pause.
func (w *recvWindow) consumed(n int64) int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buffered -= n
	return w.maybeGrantLocked()
}

// bufferedBytes returns the current buffered byte count.
func (w *recvWindow) bufferedBytes() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffered
}

// maybeGrantLocked releases the accrued returnable credit when the high-water
// gate is open and the threshold is met, returning the delta (0 otherwise). It
// must be called with w.mu held.
func (w *recvWindow) maybeGrantLocked() int64 {
	if w.buffered < w.highWater && w.pending >= w.threshold {
		g := w.pending
		w.pending = 0
		return g
	}
	return 0
}
