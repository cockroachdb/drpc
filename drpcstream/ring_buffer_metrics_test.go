// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import (
	"io"
	"sync/atomic"
	"testing"

	"github.com/zeebo/assert"

	"storj.io/drpc/drpcmetrics"
)

type ringBufferMetricValues struct {
	messages atomic.Int64
	bytes    atomic.Int64
}

type ringBufferGauge struct {
	value *atomic.Int64
}

func (g ringBufferGauge) Inc(value int64) {
	g.value.Add(value)
}

func (m *ringBufferMetricValues) bundle(enabled bool) drpcmetrics.ConnectionMetrics {
	return drpcmetrics.ConnectionMetrics{
		ShouldRecord:         func() bool { return enabled },
		ReceiveQueueMessages: ringBufferGauge{value: &m.messages},
		ReceiveQueueBytes:    ringBufferGauge{value: &m.bytes},
	}
}

func newMetricRingBuffer(metrics *ringBufferMetricValues, enabled bool) *ringBuffer {
	rb := &ringBuffer{}
	rb.init(NewBufferPool(), metrics.bundle(enabled))
	rb.buf = make([]*[]byte, 1)
	return rb
}

func TestRingBufferQueueMetrics(t *testing.T) {
	for _, tc := range []struct {
		name         string
		enabled      bool
		wantMessages int64
		wantBytes    int64
	}{
		{name: "enabled", enabled: true, wantMessages: 1, wantBytes: 3},
		{name: "disabled", enabled: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metrics := ringBufferMetricValues{}
			rb := newMetricRingBuffer(&metrics, tc.enabled)

			rb.Enqueue([]byte("one"))
			assert.Equal(t, metrics.messages.Load(), tc.wantMessages)
			assert.Equal(t, metrics.bytes.Load(), tc.wantBytes)

			data, err := rb.Dequeue()
			assert.NoError(t, err)
			assert.DeepEqual(t, data, []byte("one"))
			rb.Done()
			assert.Equal(t, metrics.messages.Load(), int64(0))
			assert.Equal(t, metrics.bytes.Load(), int64(0))

			rb.Close(io.EOF)
		})
	}
}
