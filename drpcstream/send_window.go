// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import (
	"io"
	"math"
	"sync"
	"sync/atomic"
)

// sendWindow is a per-stream flow-control credit balance on the sender. It
// tracks how many more bytes the stream is allowed to put on the wire right
// now. acquire spends credit (blocking until enough is available), grant adds
// credit, and close terminates a blocked acquire. There must be at most one
// goroutine calling acquire and one goroutine calling grant.
type sendWindow struct {
	avail atomic.Int64 // available credit; negative while acquire is waiting

	mu   sync.Mutex
	cond *sync.Cond // allocated only if acquire has to wait
	err  error      // terminal error returned by a blocked acquire after close
}

// newSendWindow returns a sendWindow seeded with initial credit.
func newSendWindow(initial int64) *sendWindow {
	w := &sendWindow{}
	w.avail.Store(initial)
	return w
}

// available returns the current credit balance.
func (w *sendWindow) available() int64 {
	return w.avail.Load()
}

// acquire debits n bytes of credit and blocks while the resulting balance is
// negative. If the window is closed while blocked, it returns the close error.
// n <= 0 is a no-op.
func (w *sendWindow) acquire(n int64) error {
	if n <= 0 {
		return nil
	}
	if w.avail.Add(-n) >= 0 {
		return nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	for w.avail.Load() < 0 && w.err == nil {
		if w.cond == nil {
			w.cond = sync.NewCond(&w.mu)
		}
		w.cond.Wait()
	}

	return w.err // can be nil
}

// grant raises the balance by n and wakes the acquirer if the grant satisfies
// its deficit. n is unsigned, so a grant never lowers the balance.
func (w *sendWindow) grant(n uint64) {
	if n == 0 {
		return
	}
	for {
		oldCredits := w.avail.Load()
		newCredits := applyGrant(oldCredits, n)
		if !w.avail.CompareAndSwap(oldCredits, newCredits) {
			continue
		}
		if oldCredits < 0 && newCredits >= 0 {
			// Taking mu makes the predicate check in acquire and this signal
			// atomic with respect to cond.Wait, preventing a lost wakeup.
			w.mu.Lock()
			if w.cond != nil {
				w.cond.Signal()
			}
			w.mu.Unlock()
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

// close terminates the window with err, waking every parked acquirer, which
// then returns err. It is a no-op if the window is already closed.
func (w *sendWindow) close(err error) {
	if err == nil {
		err = io.EOF
	}
	w.mu.Lock()
	if w.err == nil {
		w.err = err
		if w.cond != nil {
			w.cond.Signal()
		}
	}
	w.mu.Unlock()
}
