// Copyright 2015 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package drpcmetrics defines types and helpers for drpc metrics.
package drpcmetrics

import (
	"storj.io/drpc"
)

// Counter is a metric that can only be incremented (monotonically increasing).
// The labels parameter contains key-value pairs for metric dimensions
// (e.g. rpcService, rpcMethod). It may be nil when no
// dimensional context is available.
// The concrete type *must* provide a thread-safe implementation for these
// methods.
type Counter interface {
	Inc(labels map[string]string, v int64)
}

// NoOpCounter is a Counter implementation that does nothing.
type NoOpCounter struct{}

// Inc implements Counter.
func (NoOpCounter) Inc(labels map[string]string, v int64) {}

// Gauge is a metric that can increase and decrease (e.g. pool size,
// active count). Update sets the gauge to the given absolute value.
//
// Note: Gauge values may go up or down; Counter values must only increase.
// The concrete type *must* provide a thread-safe implementation for these
// methods.
type Gauge interface {
	Update(labels map[string]string, v int64)
}

// NoOpGauge is a Gauge implementation that does nothing.
type NoOpGauge struct{}

// Update implements Gauge.
func (NoOpGauge) Update(labels map[string]string, v int64) {}

// TODO (sujatha): Plug-in no-op implementation for nil metrics

// MeteredTransport wraps a Transport and increments byte counters on each
// Read and Write call.
type MeteredTransport struct {
	drpc.Transport
	BytesSent Counter
	BytesRecv Counter
}

// Read reads from the underlying transport and increments BytesRecv.
func (t *MeteredTransport) Read(p []byte) (n int, err error) {
	n, err = t.Transport.Read(p)
	if n > 0 && t.BytesRecv != nil {
		t.BytesRecv.Inc(nil, int64(n))
	}
	return n, err
}

// Write writes to the underlying transport and increments BytesSent.
func (t *MeteredTransport) Write(p []byte) (n int, err error) {
	n, err = t.Transport.Write(p)
	if n > 0 && t.BytesSent != nil {
		t.BytesSent.Inc(nil, int64(n))
	}
	return n, err
}

// ClientMetrics holds optional metrics that the client connection will
// populate during operation.
type ClientMetrics struct {
	BytesSent Counter
	BytesRecv Counter
}
