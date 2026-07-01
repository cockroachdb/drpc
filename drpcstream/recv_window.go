// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import "sync"

// recvWindow decides when the receive side returns credit to the sender: it
// withholds grants while buffered bytes are at or above the high-water mark
// (credit keeps accruing), and otherwise releases accrued credit once it
// reaches the threshold, coalescing many frames into one grant. dispatched
// (reader goroutine) and consumed (application goroutine) return the credit
// delta to send as a KindWindowUpdate and may run concurrently.
type recvWindow struct {
	mu        sync.Mutex
	buffered  int64 // in-progress reassembly + completed, not-yet-consumed bytes
	pending   int64 // returnable credit accrued but not yet granted
	highWater int64 // withhold grants while buffered is at or above this
	threshold int64 // release accrued credit once it reaches this
}

// newRecvWindow returns a recvWindow seeded with highWater and threshold.
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
// the credit delta to grant (0 if none).
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

// maybeGrantLocked releases accrued credit when the gate is open (buffered
// below high-water) and pending has reached the threshold. Requires w.mu.
func (w *recvWindow) maybeGrantLocked() int64 {
	if w.buffered < w.highWater && w.pending >= w.threshold {
		g := w.pending
		w.pending = 0
		return g
	}
	return 0
}
