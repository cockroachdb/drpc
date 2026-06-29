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
	bytesSent    Counter
	bytesRecv    Counter
	shouldRecord func() bool
}

// ToMeteredTransport returns a transport that increments bytesSent and
// bytesRecv on each Write and Read call respectively. Nil counters are
// replaced with no-op implementations.
func ToMeteredTransport(
	tr drpc.Transport, bytesSent,
	bytesRecv Counter, shouldRecord func() bool,
) drpc.Transport {
	if bytesSent == nil {
		bytesSent = NoOpCounter{}
	}
	if bytesRecv == nil {
		bytesRecv = NoOpCounter{}
	}
	return &meteredTransport{Transport: tr, bytesSent: bytesSent,
		bytesRecv: bytesRecv, shouldRecord: shouldRecord}
}

// Read reads from the underlying transport and increments bytesRecv.
func (t *meteredTransport) Read(p []byte) (n int, err error) {
	n, err = t.Transport.Read(p)
	if n > 0 && t.shouldRecord() {
		t.bytesRecv.Inc(int64(n))
	}
	return n, err
}

// Write writes to the underlying transport and increments bytesSent.
func (t *meteredTransport) Write(p []byte) (n int, err error) {
	n, err = t.Transport.Write(p)
	if n > 0 && t.shouldRecord() {
		t.bytesSent.Inc(int64(n))
	}
	return n, err
}

// ConnectionMetrics holds metrics populated by a DRPC connection. The caller
// binds each handle to its desired labels before passing the bundle to DRPC.
// Its zero value records nothing.
type ConnectionMetrics struct {
	BytesSent Counter
	BytesRecv Counter

	StreamsStarted    Counter
	StreamsTerminated Counter
}

// WithDefaults returns a copy with nil metric handles replaced by no-op
// implementations.
func (m ConnectionMetrics) WithDefaults() ConnectionMetrics {
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
	return m
}
