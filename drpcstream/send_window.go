// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import (
	"context"
	"math"
	"sync"
)

// sendWindow is a per-stream flow-control credit balance on the sender. It
// tracks how many more bytes the stream is allowed to put on the wire right
// now. acquire spends credit (blocking until enough is available), grant adds
// credit, and close terminates the window.
//
// Grants are no-revoke: grant is strictly additive, and the receiver never
// takes back credit it has already issued. The balance is a signed int64;
// acquire never drives it below zero, but the enablement layer may debit credit
// for bytes sent before flow control begins enforcing, leaving it negative, in
// which case acquire blocks until grants restore it.
type sendWindow struct {
	mu     sync.Mutex
	avail  int64         // available credit; signed (may be negative — see doc)
	closed bool          // set once by close; no further acquires succeed
	err    error         // terminal error returned by acquire after close
	notify chan struct{} // closed+replaced to wake parked acquirers
}

// newSendWindow returns a sendWindow seeded with initial credit.
func newSendWindow(initial int64) *sendWindow {
	return &sendWindow{avail: initial, notify: make(chan struct{})}
}

// available returns the current credit balance.
func (w *sendWindow) available() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.avail
}

// acquire blocks until n bytes of credit are available, then debits them and
// returns nil. It returns early if the window is closed (with the close error)
// or ctx is canceled (with ctx.Err()), consuming no credit in either case. Both
// are checked before any debit, so a canceled context never consumes credit or
// lets a frame proceed even when credit is available.
func (w *sendWindow) acquire(ctx context.Context, n int64) error {
	for {
		w.mu.Lock()
		switch {
		case w.closed:
			err := w.err
			w.mu.Unlock()
			return err
		case ctx.Err() != nil:
			w.mu.Unlock()
			return ctx.Err()
		case w.avail >= n:
			w.avail -= n
			w.mu.Unlock()
			return nil
		}
		// Snapshot the notify channel under the lock before parking, so a grant
		// or close that fires the instant we unlock is not missed.
		ch := w.notify
		w.mu.Unlock()

		select {
		case <-ch:
			// Credit was granted or the window closed; loop and re-check.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// grant adds n bytes of credit and wakes any parked acquirer. n is unsigned
// (the wire delta type), so a grant can only ever raise the balance — no-revoke.
// The addition saturates at math.MaxInt64 so a large or malicious delta can
// never wrap the balance negative. Grants after close are ignored (the window
// is dead).
func (w *sendWindow) grant(n uint64) {
	w.mu.Lock()
	if !w.closed {
		if n >= uint64(math.MaxInt64) {
			w.avail = math.MaxInt64
		} else if sum := w.avail + int64(n); sum < w.avail {
			w.avail = math.MaxInt64 // overflowed past MaxInt64
		} else {
			w.avail = sum
		}
		w.wakeLocked()
	}
	w.mu.Unlock()
}

// close terminates the window with err, waking every parked acquirer, which
// then returns err. Subsequent acquires also return err. It is a no-op if the
// window is already closed.
func (w *sendWindow) close(err error) {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		w.err = err
		w.wakeLocked()
	}
	w.mu.Unlock()
}

// wakeLocked broadcasts to all parked acquirers by closing the current notify
// channel and installing a fresh one. It must be called with w.mu held.
func (w *sendWindow) wakeLocked() {
	close(w.notify)
	w.notify = make(chan struct{})
}
