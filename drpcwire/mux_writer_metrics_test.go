// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcwire

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeebo/assert"
	"storj.io/drpc/drpcmetrics"
)

type writerMetricValues struct {
	queueBytes      atomic.Int64
	blockedWriters  atomic.Int64
	queueBlockCount atomic.Int64
	inFlightBytes   atomic.Int64
}

func (m *writerMetricValues) bundle() drpcmetrics.ConnectionMetrics {
	return drpcmetrics.ConnectionMetrics{
		ShouldRecord:             func() bool { return true },
		WriteQueueBytes:          writerGauge{&m.queueBytes},
		WriteQueueBlockedWriters: writerGauge{&m.blockedWriters},
		WriteQueueBlockCount:     writerCounter{&m.queueBlockCount},
		WriteFlushInFlightBytes:  writerGauge{&m.inFlightBytes},
	}
}

type writerCounter struct{ n *atomic.Int64 }

func (c writerCounter) Inc(v int64) { c.n.Add(v) }

type writerGauge struct{ n *atomic.Int64 }

func (g writerGauge) Inc(v int64) { g.n.Add(v) }

func waitForWriterMetric(t *testing.T, value *atomic.Int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for value.Load() != want {
		if time.Now().After(deadline) {
			t.Fatalf("metric = %d, want %d", value.Load(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestMuxWriterWriteMetrics(t *testing.T) {
	bw := newBlockingWriter()
	var metrics writerMetricValues
	mw := NewMuxWriterWithOptions(bw, func(error) {}, metrics.bundle(), WriterOptions{MaximumBufferSize: 1})

	// The first frame is in Write, the second fills the pending queue, and the
	// third parks on backpressure.
	blockUntilFull(t, mw, bw)
	waitForWriterMetric(t, &metrics.queueBlockCount, 0)
	assert.That(t, metrics.inFlightBytes.Load() > 0)
	assert.That(t, metrics.queueBytes.Load() > 0)

	done := make(chan error, 1)
	go func() { done <- mw.WriteFrame(RandFrame(), nil) }()
	waitForWriterMetric(t, &metrics.blockedWriters, 1)
	waitForWriterMetric(t, &metrics.queueBlockCount, 1)

	close(bw.unblock)
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("parked WriteFrame stayed blocked after drain")
	}

	waitForWriterMetric(t, &metrics.blockedWriters, 0)
	waitForWriterMetric(t, &metrics.queueBytes, 0)
	waitForWriterMetric(t, &metrics.inFlightBytes, 0)
	assert.Equal(t, metrics.queueBlockCount.Load(), int64(1))

	mw.Stop(errors.New("stopped"))
	<-mw.Done()
}

func TestMuxWriterWriteMetricsOnStop(t *testing.T) {
	bw := newBlockingWriter()
	var metrics writerMetricValues
	mw := NewMuxWriterWithOptions(bw, func(error) {}, metrics.bundle(), WriterOptions{MaximumBufferSize: 1})

	blockUntilFull(t, mw, bw)
	assert.That(t, metrics.queueBytes.Load() > 0)
	assert.That(t, metrics.inFlightBytes.Load() > 0)

	mw.Stop(errors.New("stopped"))
	waitForWriterMetric(t, &metrics.queueBytes, 0)

	close(bw.unblock)
	<-mw.Done()
	waitForWriterMetric(t, &metrics.inFlightBytes, 0)
}

func TestMuxWriterWriteMetricsOnWriteFailure(t *testing.T) {
	bw := newBlockingWriter()
	bw.err = errors.New("write failed")
	var metrics writerMetricValues
	mw := NewMuxWriterWithOptions(bw, func(error) {}, metrics.bundle(), WriterOptions{MaximumBufferSize: 1})

	blockUntilFull(t, mw, bw)
	close(bw.unblock)
	<-mw.Done()

	waitForWriterMetric(t, &metrics.queueBytes, 0)
	waitForWriterMetric(t, &metrics.inFlightBytes, 0)
}
