// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import (
	"math"
	"sync/atomic"
)

// sendWindow is a per-stream flow-control credit balance on the sender. It
// tracks how many more bytes the stream may put on the wire. acquire spends
// credit, blocking while the balance is negative (insufficient); debit spends
// without blocking (the overdraft path); grant adds credit and wakes a blocked
// acquire once the deficit is repaid.
//
// Termination is delegated to the done channel supplied at
// construction -- the stream's send signal.
type sendWindow struct {
	avail atomic.Int64    // credit; goes negative from a parked acquire or an overdraft debit
	ch    chan struct{}   // grant signals here, acquire waits on it
	done  <-chan struct{} // closed to abort a blocked (or future) acquire
	err   func() error    // terminal error, consulted only when done is closed
}

// newSendWindow returns a sendWindow seeded with initial credit. Closing done
// aborts acquire, which then returns err().
func newSendWindow(initial int64, done <-chan struct{}, err func() error) *sendWindow {
	w := &sendWindow{ch: make(chan struct{}, 1), done: done, err: err}
	w.avail.Store(initial)
	return w
}

// available returns the current credit balance.
func (w *sendWindow) available() int64 { return w.avail.Load() }

// acquire debits n bytes of credit, blocking while the resulting balance is
// negative, and returns nil once it is covered. A blocked acquire returns err()
// when done is closed. n <= 0 is a no-op.
func (w *sendWindow) acquire(n int64) error {
	if n <= 0 || w.avail.Add(-n) >= 0 {
		return nil
	}
	for {
		select {
		case <-w.done:
			return w.err()
		case <-w.ch:
			if w.avail.Load() >= 0 {
				return nil
			}
		}
	}
}

// debit spends n credit without blocking, letting the balance go negative. It
// is the overdraft path: once a sender has committed to a message (acquired
// credit for its first frame), it debits the remaining frames without parking so
// a message larger than the window completes instead of deadlocking. The
// overdraft is repaid by later grants (applyGrant clears the deficit first) and
// is bounded by the caller to MaxMessageSize. n <= 0 is a no-op.
func (w *sendWindow) debit(n int64) {
	if n > 0 {
		w.avail.Add(-n)
	}
}

// grant raises the balance by n and, when that repays a waiting acquire's
// deficit, wakes it. n is the wire delta (unsigned), so a grant never lowers the
// balance.
func (w *sendWindow) grant(n uint64) {
	if n == 0 {
		return
	}
	for {
		old := w.avail.Load()
		next := applyGrant(old, n)
		if !w.avail.CompareAndSwap(old, next) {
			continue
		}
		if old < 0 && next >= 0 {
			// The deficit just cleared, so unblock a blocked acquire.
			// A signal with no waiter yet stays buffered until it is consumed,
			// so the wakeup is never lost. A later grant that also tries to
			// deposit a token before it is consumed, just drops it. This is
			// okay because when acquire finally wakes, it reads the current
			// avail, which reflects every grant that happened so far.
			select {
			case w.ch <- struct{}{}:
			default:
			}
		}
		return
	}
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
