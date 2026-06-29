// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import (
	"io"
	"sync/atomic"
	"testing"

	"github.com/zeebo/assert"
)

// TestRingBuffer_OnBlockFiresOnceWhenFull verifies that the onBlock hook fires
// exactly once each time an Enqueue finds the buffer full and must wait, and
// not at all for enqueues that proceed without blocking.
func TestRingBuffer_OnBlockFiresOnceWhenFull(t *testing.T) {
	var blocks atomic.Int64
	fired := make(chan struct{}, 1)

	var rb ringBuffer
	rb.cond.L = &rb.mu
	rb.pool = NewBufferPool()
	rb.buf = make([]*[]byte, 1) // capacity 1 keeps the test deterministic
	rb.onBlock = func() {
		blocks.Add(1)
		select {
		case fired <- struct{}{}:
		default:
		}
	}

	// Filling the only slot does not block and must not fire the hook.
	rb.Enqueue([]byte("a"))
	assert.Equal(t, blocks.Load(), int64(0))

	// A second enqueue finds the buffer full and parks; the hook fires once.
	done := make(chan struct{})
	go func() {
		rb.Enqueue([]byte("b"))
		close(done)
	}()
	<-fired
	assert.Equal(t, blocks.Load(), int64(1))

	// Draining a slot lets the parked producer complete. The producer wakes,
	// re-checks the (now non-full) buffer, and enqueues without firing again.
	data, err := rb.Dequeue()
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("a"))
	<-done
	assert.Equal(t, blocks.Load(), int64(1))

	rb.Close(io.EOF)
}
