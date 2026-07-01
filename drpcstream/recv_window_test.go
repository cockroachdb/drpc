// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import (
	"testing"

	"github.com/zeebo/assert"
)

// Below the high-water mark, accrued returnable credit is released as a grant
// once it reaches the threshold, and withheld (0) below it.
func TestRecvWindowGrantsAtThreshold(t *testing.T) {
	w := newRecvWindow(1000, 100)
	assert.Equal(t, w.dispatched(60), int64(0))   // pending 60 < 100
	assert.Equal(t, w.dispatched(60), int64(120)) // pending 120 >= 100, buffered 120 < 1000
	assert.Equal(t, w.bufferedBytes(), int64(120))
	assert.Equal(t, w.dispatched(50), int64(0)) // pending reset by the grant; 50 < 100
}

// At or above the high-water mark, grants are withheld even when the accrued
// credit is past the threshold.
func TestRecvWindowWithholdsAtOrAboveHighWater(t *testing.T) {
	w := newRecvWindow(100, 50)
	assert.Equal(t, w.dispatched(120), int64(0)) // buffered 120 >= 100 -> withhold
	assert.Equal(t, w.bufferedBytes(), int64(120))
}

// Consuming buffered data drops below the high-water mark and flushes the
// credit accrued while the gate was closed (resume-on-consume).
func TestRecvWindowResumesOnConsume(t *testing.T) {
	w := newRecvWindow(100, 50)
	assert.Equal(t, w.dispatched(120), int64(0)) // withheld; pending 120
	assert.Equal(t, w.consumed(30), int64(120))  // buffered 90 < 100 -> flush pending
	assert.Equal(t, w.bufferedBytes(), int64(90))
}

// Consuming while the accrued credit is below the threshold emits no grant.
func TestRecvWindowConsumeBelowThresholdNoGrant(t *testing.T) {
	w := newRecvWindow(1000, 100)
	assert.Equal(t, w.dispatched(50), int64(0)) // pending 50 < 100
	assert.Equal(t, w.consumed(20), int64(0))   // buffered 30, pending 50 < 100
	assert.Equal(t, w.bufferedBytes(), int64(30))
}

// Many small dispatches coalesce into few grants, each carrying the accrued
// amount.
func TestRecvWindowCoalescesGrants(t *testing.T) {
	w := newRecvWindow(1_000_000, 100)
	grants, granted := 0, int64(0)
	for i := 0; i < 10; i++ {
		if g := w.dispatched(30); g > 0 {
			grants++
			granted += g
		}
	}
	// 10 dispatches of 30 (300 bytes) with threshold 100 coalesce into 2 grants
	// of 120 each; the remaining 60 stays accrued.
	assert.Equal(t, grants, 2)
	assert.Equal(t, granted, int64(240))
}
