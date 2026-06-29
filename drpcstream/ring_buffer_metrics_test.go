// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import (
	"io"
	"sync/atomic"
	"testing"

	"github.com/zeebo/assert"
)

type ringBufferMetricValues struct {
	messages atomic.Int64
	bytes    atomic.Int64
}

func (m *ringBufferMetricValues) hooks() ringBufferHooks {
	return ringBufferHooks{
		onEnqueue: func(bytes int64) {
			m.messages.Add(1)
			m.bytes.Add(bytes)
		},
		onDequeue: func(bytes int64) {
			m.messages.Add(-1)
			m.bytes.Add(-bytes)
		},
	}
}

func newMetricRingBuffer(metrics *ringBufferMetricValues) *ringBuffer {
	rb := &ringBuffer{}
	rb.cond.L = &rb.mu
	rb.pool = NewBufferPool()
	rb.buf = make([]*[]byte, 1)
	rb.hooks = metrics.hooks()
	return rb
}

func TestRingBufferQueueMetrics(t *testing.T) {
	metrics := ringBufferMetricValues{}
	rb := newMetricRingBuffer(&metrics)

	rb.Enqueue([]byte("one"))
	assert.Equal(t, metrics.messages.Load(), int64(1))
	assert.Equal(t, metrics.bytes.Load(), int64(3))
	data, err := rb.Dequeue()
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("one"))
	rb.Done()
	assert.Equal(t, metrics.messages.Load(), int64(0))
	assert.Equal(t, metrics.bytes.Load(), int64(0))

	rb.Close(io.EOF)
}
