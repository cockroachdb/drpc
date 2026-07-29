// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

// recvWindow is the receive side of per-stream flow control. It returns credit
// to the sender as the application consumes received bytes (grant-on-consume),
// coalescing consumes into a single grant once the accrued amount reaches the
// threshold, bounding window-update frequency to one per threshold bytes.
type recvWindow struct {
	pending   int64 // bytes consumed since the last grant
	threshold int64 // release the accrued credit once it reaches this
}

// newRecvWindow returns a recvWindow that coalesces grants until threshold.
// The threshold must be small relative to the sender's window, or the withheld
// credit can stall the sender.
func newRecvWindow(threshold int64) *recvWindow {
	return &recvWindow{threshold: threshold}
}

// consumed accrues n consumed bytes and returns the credit to grant once the
// accrued amount reaches the threshold, or 0 to keep coalescing.
func (w *recvWindow) consumed(n int64) int64 {
	w.pending += n
	if w.pending >= w.threshold {
		g := w.pending
		w.pending = 0
		return g
	}
	return 0
}
