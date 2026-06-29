// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcmetrics

import "testing"

// countingCounter is a Counter that accumulates into an int64 so tests can
// observe that a handle stored in the bundle is the one actually incremented.
type countingCounter struct{ n *int64 }

func (c countingCounter) Inc(v int64) { *c.n += v }

func TestMuxMetricsWithDefaults(t *testing.T) {
	// A nil receiver yields an all-no-op bundle whose handles are safe to call.
	m := (*MuxMetrics)(nil).WithDefaults()
	m.StreamsOpened.Inc(1)
	m.StreamsClosed.Inc(1)
	m.StreamsFailed.Inc(1)
	if !m.ShouldRecord() {
		t.Fatal("default ShouldRecord must return true")
	}

	// Provided fields are preserved and reach the underlying handle; missing
	// fields are filled with no-ops that must not panic.
	var got int64
	in := &MuxMetrics{StreamsOpened: countingCounter{&got}}
	out := in.WithDefaults()
	out.StreamsOpened.Inc(2)
	out.StreamsClosed.Inc(1) // no-op
	out.StreamsFailed.Inc(1) // no-op
	if got != 2 {
		t.Fatalf("expected provided counter to observe 2, got %d", got)
	}

	// A provided ShouldRecord is preserved rather than overwritten.
	off := (&MuxMetrics{ShouldRecord: func() bool { return false }}).WithDefaults()
	if off.ShouldRecord() {
		t.Fatal("provided ShouldRecord must be preserved")
	}
}
