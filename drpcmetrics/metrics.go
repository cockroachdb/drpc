// Copyright 2015 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package drpcmetrics defines types and helpers for drpc metrics.
package drpcmetrics

import "storj.io/drpc"

// Counter is a metric that can only be incremented (monotonically increasing).
// The concrete type must provide a thread-safe implementation for the method.
type Counter interface {
	Inc(v int64)
}

// NoOpCounter is a Counter implementation that does nothing.
type NoOpCounter struct{}

// Inc implements Counter.
func (NoOpCounter) Inc(v int64) {}

// LabeledCounter is a Counter that accepts dimensional labels on each
// increment. The labels parameter contains key-value pairs for metric
// dimensions. It may be nil when no dimensional context is available.
// The concrete type must provide a thread-safe implementation.
type LabeledCounter interface {
	Inc(labels map[string]string, v int64)
}

// NoOpLabeledCounter is a LabeledCounter implementation that does nothing.
type NoOpLabeledCounter struct{}

// Inc implements LabeledCounter.
func (NoOpLabeledCounter) Inc(labels map[string]string, v int64) {}

// Gauge is a metric that can increase and decrease (e.g. pool size).
// Update sets the gauge to the given absolute value.
//
// Note: Gauge values may go up or down; Counter values must only increase.
// The concrete type must provide a thread-safe implementation for the
// method.
type Gauge interface {
	Update(labels map[string]string, v int64)
}

// NoOpGauge is a Gauge implementation that does nothing.
type NoOpGauge struct{}

// Update implements Gauge.
func (NoOpGauge) Update(labels map[string]string, v int64) {}

// AdditiveGauge is a metric that tracks current state by applying positive and
// negative deltas. The concrete type must provide a thread-safe implementation.
type AdditiveGauge interface {
	Inc(v int64)
}

// NoOpAdditiveGauge is an AdditiveGauge implementation that does nothing.
type NoOpAdditiveGauge struct{}

// Inc implements AdditiveGauge.
func (NoOpAdditiveGauge) Inc(v int64) {}

// meteredTransport wraps a Transport and increments byte counters on each
// Read and Write call.
type meteredTransport struct {
	drpc.Transport
	metrics ConnectionMetrics
}

// ToMeteredTransport returns a transport that records bytes read and written
// through metrics.
func ToMeteredTransport(tr drpc.Transport, metrics ConnectionMetrics) drpc.Transport {
	return &meteredTransport{Transport: tr, metrics: metrics.WithDefaults()}
}

// Read reads from the underlying transport and increments bytesRecv.
func (t *meteredTransport) Read(p []byte) (n int, err error) {
	n, err = t.Transport.Read(p)
	if n > 0 && t.metrics.ShouldRecord() {
		t.metrics.BytesRecv.Inc(int64(n))
	}
	return n, err
}

// Write writes to the underlying transport and increments bytesSent.
func (t *meteredTransport) Write(p []byte) (n int, err error) {
	n, err = t.Transport.Write(p)
	if n > 0 && t.metrics.ShouldRecord() {
		t.metrics.BytesSent.Inc(int64(n))
	}
	return n, err
}

// ConnectionMetrics controls metrics for one DRPC connection. The caller binds
// each handle to its desired labels before passing the bundle to DRPC. Its zero
// value records nothing.
type ConnectionMetrics struct {
	// ShouldRecord controls whether this connection records metrics. A nil
	// function disables collection.
	//
	// TODO: Evaluate how changes to ShouldRecord should affect stateful gauges.
	// Checking it independently at paired lifecycle or queue events can leave a
	// gauge stale or negative.
	ShouldRecord func() bool

	BytesSent Counter
	BytesRecv Counter

	StreamsStarted    Counter
	StreamsTerminated Counter

	ReceiveQueueMessages AdditiveGauge
	ReceiveQueueBytes    AdditiveGauge

	WriteQueueBytes          AdditiveGauge
	WriteQueueBlockedWriters AdditiveGauge
	WriteQueueBlockCount     Counter
	WriteFlushInFlightBytes  AdditiveGauge
}

// WithDefaults returns a copy with nil metric handles replaced by no-op
// implementations.
func (m ConnectionMetrics) WithDefaults() ConnectionMetrics {
	if m.ShouldRecord == nil {
		m.ShouldRecord = neverRecord
	}
	if m.BytesSent == nil {
		m.BytesSent = NoOpCounter{}
	}
	if m.BytesRecv == nil {
		m.BytesRecv = NoOpCounter{}
	}
	if m.StreamsStarted == nil {
		m.StreamsStarted = NoOpCounter{}
	}
	if m.StreamsTerminated == nil {
		m.StreamsTerminated = NoOpCounter{}
	}
	if m.ReceiveQueueMessages == nil {
		m.ReceiveQueueMessages = NoOpAdditiveGauge{}
	}
	if m.ReceiveQueueBytes == nil {
		m.ReceiveQueueBytes = NoOpAdditiveGauge{}
	}
	if m.WriteQueueBytes == nil {
		m.WriteQueueBytes = NoOpAdditiveGauge{}
	}
	if m.WriteQueueBlockedWriters == nil {
		m.WriteQueueBlockedWriters = NoOpAdditiveGauge{}
	}
	if m.WriteQueueBlockCount == nil {
		m.WriteQueueBlockCount = NoOpCounter{}
	}
	if m.WriteFlushInFlightBytes == nil {
		m.WriteFlushInFlightBytes = NoOpAdditiveGauge{}
	}
	return m
}

func neverRecord() bool { return false }
