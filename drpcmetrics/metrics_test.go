// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcmetrics

import "testing"

// countingCounter is a Counter that accumulates into an int64 so tests can
// observe that a handle stored in the bundle is the one actually incremented.
type countingCounter struct{ n *int64 }

func (c countingCounter) Inc(v int64) { *c.n += v }

func TestConnectionMetricsWithDefaults(t *testing.T) {
	// The zero value yields an all-no-op bundle whose handles are safe to call.
	m := (ConnectionMetrics{}).WithDefaults()
	m.BytesSent.Inc(1)
	m.BytesRecv.Inc(1)
	m.StreamsStarted.Inc(1)
	m.StreamsTerminated.Inc(1)
	m.ReceiveQueueMessages.Inc(1)
	m.ReceiveQueueMessages.Inc(-1)
	m.ReceiveQueueBytes.Inc(1)
	m.ReceiveQueueBytes.Inc(-1)
	m.WriteQueueBytes.Inc(1)
	m.WriteQueueBytes.Inc(-1)
	m.WriteQueueBlockedWriters.Inc(1)
	m.WriteQueueBlockedWriters.Inc(-1)
	m.WriteQueueBlockCount.Inc(1)
	m.WriteFlushInFlightBytes.Inc(1)
	m.WriteFlushInFlightBytes.Inc(-1)
	if m.ShouldRecord() {
		t.Fatal("default metric bundle records metrics")
	}

	// Provided fields are preserved and reach the underlying handle; missing
	// fields are filled with no-ops that must not panic.
	var got int64
	in := ConnectionMetrics{
		ShouldRecord:   func() bool { return true },
		StreamsStarted: countingCounter{&got},
	}
	out := in.WithDefaults()
	if !out.ShouldRecord() {
		t.Fatal("provided recording gate was not preserved")
	}
	out.StreamsStarted.Inc(2)
	out.StreamsTerminated.Inc(1) // no-op
	out.ReceiveQueueMessages.Inc(1)
	out.ReceiveQueueBytes.Inc(1)
	out.WriteQueueBytes.Inc(1)
	out.WriteQueueBlockedWriters.Inc(1)
	out.WriteQueueBlockCount.Inc(1)
	out.WriteFlushInFlightBytes.Inc(1)
	out.BytesSent.Inc(1) // no-op
	out.BytesRecv.Inc(1) // no-op
	if got != 2 {
		t.Fatalf("expected provided counter to observe 2, got %d", got)
	}
}
