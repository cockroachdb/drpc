// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import (
	"io"
	"math"
	"sync"
)

// sendWindow is a per-stream flow-control credit balance on the sender. It
// tracks how many more bytes the stream is allowed to put on the wire right
// now. acquire spends credit (blocking until enough is available), grant adds
// credit, and close terminates the window.
type sendWindow struct {
	mu     sync.Mutex
	avail  int64         // available credit; signed (maybe negative during enablement)
	closed bool          // set once by close; no further acquires succeed
	err    error         // terminal error returned by acquire after close
	notify chan struct{} // lazily allocated by parkers; closed+nilled to wake them
}

// newSendWindow returns a sendWindow seeded with initial credit.
func newSendWindow(initial int64) *sendWindow {
	return &sendWindow{avail: initial}
}

// available returns the current credit balance.
func (w *sendWindow) available() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.avail
}

// acquire debits n bytes of credit, blocking until available, and returns nil.
// It returns early (consuming no credit) with the close error if the window is
// closed. n <= 0 is a no-op unless the window is already closed.
func (w *sendWindow) acquire(n int64) error {
	for {
		w.mu.Lock()
		switch {
		case w.closed:
			// Checked before n <= 0 so a closed window still fails an empty frame.
			err := w.err
			w.mu.Unlock()
			return err
		case n <= 0:
			w.mu.Unlock() // nothing to acquire; never debits, so a negative n adds no credit
			return nil
		case w.avail >= n:
			w.avail -= n
			w.mu.Unlock()
			return nil
		}
		// Snapshot the notify channel under the lock before parking, so a grant
		// or close that fires the instant we unlock is not missed. Allocated
		// here, by the first parker, so wakes with no waiters stay free. The
		// only wakeups are grant and close; abort is close's job (the stream
		// terminates the window), so acquire needs no context.
		if w.notify == nil {
			w.notify = make(chan struct{})
		}
		ch := w.notify
		w.mu.Unlock()
		<-ch // credit was granted or the window closed; loop and re-check
	}
}

// grant raises the balance by n and wakes any parked acquirer. n is unsigned,
// so a grant never lowers the balance. Grants after close are ignored.
func (w *sendWindow) grant(n uint64) {
	w.mu.Lock()
	if !w.closed {
		w.avail = applyGrant(w.avail, n)
		w.wakeLocked()
	}
	w.mu.Unlock()
}

// applyGrant returns avail + n with an upper bound of math.MaxInt64.
// n is the wire delta (unsigned), so the result never drops below avail.
func applyGrant(avail int64, n uint64) int64 {
	if avail >= 0 {
		if n > uint64(math.MaxInt64-avail) {
			return math.MaxInt64
		}
		return avail + int64(n)
	}
	// A negative avail (before FC enablement) is repaid before any positive
	// credit starts accruing.
	deficit := uint64(-avail) // |avail| as uint64; -math.MinInt64 wraps to its magnitude
	if n <= deficit {
		return -int64(deficit - n) // debt only partly repaid; result still <= 0
	}
	if rem := n - deficit; rem <= uint64(math.MaxInt64) {
		return int64(rem)
	}
	return math.MaxInt64
}

// close terminates the window with err, waking every parked acquirer, which
// then returns err. Subsequent acquires also return err. It is a no-op if the
// window is already closed.
func (w *sendWindow) close(err error) {
	if err == nil {
		err = io.EOF // never nil: acquire must not report success on a closed window
	}
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		w.err = err
		w.wakeLocked()
	}
	w.mu.Unlock()
}

// wakeLocked broadcasts to all parked acquirers by closing the notify channel;
// Requires w.mu.
func (w *sendWindow) wakeLocked() {
	if w.notify != nil {
		close(w.notify)
		w.notify = nil
	}
}
