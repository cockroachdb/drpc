// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import (
	"io"
	"sync"
	"testing"

	"github.com/zeebo/assert"

	"storj.io/drpc/drpcmetrics"
)

func TestRingBuffer_EnqueueDequeue(t *testing.T) {
	var rb ringBuffer
	rb.init(NewBufferPool(), drpcmetrics.ConnectionMetrics{})

	rb.Enqueue([]byte("hello"))

	data, err := rb.Dequeue()
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("hello"))
}

func TestRingBuffer_FIFO(t *testing.T) {
	var rb ringBuffer
	rb.init(NewBufferPool(), drpcmetrics.ConnectionMetrics{})

	rb.Enqueue([]byte("first"))
	rb.Enqueue([]byte("second"))
	rb.Enqueue([]byte("third"))

	for _, want := range []string{"first", "second", "third"} {
		data, err := rb.Dequeue()
		assert.NoError(t, err)
		assert.DeepEqual(t, data, []byte(want))
	}
}

func TestRingBuffer_DequeueBlocksUntilEnqueue(t *testing.T) {
	var rb ringBuffer
	rb.init(NewBufferPool(), drpcmetrics.ConnectionMetrics{})

	got := make(chan []byte, 1)
	go func() {
		data, err := rb.Dequeue()
		assert.NoError(t, err)
		got <- data
	}()

	rb.Enqueue([]byte("delayed"))
	assert.DeepEqual(t, <-got, []byte("delayed"))
}

func TestRingBuffer_EnqueueBlocksWhenFull(t *testing.T) {
	var rb ringBuffer
	rb.init(NewBufferPool(), drpcmetrics.ConnectionMetrics{})
	rb.buf = make([]*[]byte, 2) // capacity 2

	rb.Enqueue([]byte("a"))
	rb.Enqueue([]byte("b"))

	// Third enqueue should block until we drain one.
	done := make(chan struct{})
	go func() {
		rb.Enqueue([]byte("c"))
		close(done)
	}()

	// Drain one slot.
	data, err := rb.Dequeue()
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("a"))

	// Now the blocked Enqueue should complete.
	<-done

	// Verify remaining items.
	data, err = rb.Dequeue()
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("b"))

	data, err = rb.Dequeue()
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("c"))
}

func TestRingBuffer_CloseUnblocksDequeue(t *testing.T) {
	var rb ringBuffer
	rb.init(NewBufferPool(), drpcmetrics.ConnectionMetrics{})

	errch := make(chan error, 1)
	go func() {
		_, err := rb.Dequeue()
		errch <- err
	}()

	rb.Close(io.EOF)
	assert.Equal(t, <-errch, io.EOF)
}

func TestRingBuffer_CloseUnblocksEnqueue(t *testing.T) {
	var rb ringBuffer
	rb.init(NewBufferPool(), drpcmetrics.ConnectionMetrics{})
	rb.buf = make([]*[]byte, 1) // capacity 1

	rb.Enqueue([]byte("fill"))

	done := make(chan struct{})
	go func() {
		rb.Enqueue([]byte("blocked"))
		close(done)
	}()

	rb.Close(io.EOF)
	<-done
}

func TestRingBuffer_CloseDrainsQueued(t *testing.T) {
	var rb ringBuffer
	rb.init(NewBufferPool(), drpcmetrics.ConnectionMetrics{})

	rb.Enqueue([]byte("queued"))
	rb.Close(io.EOF)

	// Dequeue returns the queued data first.
	data, err := rb.Dequeue()
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("queued"))

	// Next Dequeue returns the close error.
	data, err = rb.Dequeue()
	assert.Nil(t, data)
	assert.Equal(t, err, io.EOF)
}

func TestRingBuffer_CloseIdempotent(t *testing.T) {
	var rb ringBuffer
	rb.init(NewBufferPool(), drpcmetrics.ConnectionMetrics{})

	rb.Close(io.EOF)
	rb.Close(io.ErrUnexpectedEOF) // should not overwrite

	_, err := rb.Dequeue()
	assert.Equal(t, err, io.EOF) // original error preserved
}

func TestRingBuffer_EnqueueAfterClose(t *testing.T) {
	var rb ringBuffer
	rb.init(NewBufferPool(), drpcmetrics.ConnectionMetrics{})

	rb.Close(io.EOF)
	rb.Enqueue([]byte("dropped")) // should not panic or block
}

func TestRingBuffer_SlotReuse(t *testing.T) {
	var rb ringBuffer
	rb.init(NewBufferPool(), drpcmetrics.ConnectionMetrics{})
	rb.buf = make([]*[]byte, 2)

	// Fill and drain a few rounds to exercise slot reuse.
	for round := 0; round < 5; round++ {
		rb.Enqueue([]byte("data"))
		data, err := rb.Dequeue()
		assert.NoError(t, err)
		assert.DeepEqual(t, data, []byte("data"))
	}
}

func TestRingBuffer_ConcurrentProducerConsumer(t *testing.T) {
	var rb ringBuffer
	rb.init(NewBufferPool(), drpcmetrics.ConnectionMetrics{})

	const n = 1000
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			rb.Enqueue([]byte{byte(i)})
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			data, err := rb.Dequeue()
			assert.NoError(t, err)
			assert.Equal(t, (data)[0], byte(i))
		}
	}()

	wg.Wait()
	rb.Close(io.EOF)
}

// Under a byte budget the producer is not blocked on overrun: a message that
// would exceed the budget is rejected (Enqueue returns false) so the caller can
// fail-stop instead of stalling the shared reader. A dispatch frees room again.
func TestRingBuffer_ByteBudgetRejectsOverrun(t *testing.T) {
	var rb ringBuffer
	rb.init(NewBufferPool(), drpcmetrics.ConnectionMetrics{})
	rb.setMaxBytes(8) // room for two 4-byte messages

	assert.That(t, rb.Enqueue([]byte("aaaa"))) // bytes = 4
	assert.That(t, rb.Enqueue([]byte("bbbb"))) // bytes = 8, at budget

	// Third message overruns the budget: rejected, not blocked.
	assert.That(t, !rb.Enqueue([]byte("cccc")))

	// Dispatching one message frees room, and the message is admitted.
	data, err := rb.Dequeue()
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("aaaa"))
	assert.That(t, rb.Enqueue([]byte("cccc")))

	for _, want := range []string{"bbbb", "cccc"} {
		data, err := rb.Dequeue()
		assert.NoError(t, err)
		assert.DeepEqual(t, data, []byte(want))
	}
}

// The incoming message's length is part of the admission check, so a message is
// rejected before the queue overshoots its budget.
func TestRingBuffer_ByteBudgetIncludesIncomingLength(t *testing.T) {
	var rb ringBuffer
	rb.init(NewBufferPool(), drpcmetrics.ConnectionMetrics{})
	rb.setMaxBytes(8)

	assert.That(t, rb.Enqueue([]byte("aaaaaaa"))) // 7 bytes, empty -> accepted
	assert.Equal(t, rb.bytes, int64(7))

	// 7 + 4 = 11 > 8, so the 4-byte message is rejected even though the 7 bytes
	// already queued are under budget.
	assert.That(t, !rb.Enqueue([]byte("bbbb")))

	data, err := rb.Dequeue() // frees 7 bytes
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("aaaaaaa"))
	assert.That(t, rb.Enqueue([]byte("bbbb"))) // now 0 + 4 <= 8, admitted
}

// Byte accounting is released on dispatch (Dequeue), matching where credit
// grants fire, not on Done.
func TestRingBuffer_ByteAccountReleasedOnDequeue(t *testing.T) {
	var rb ringBuffer
	rb.init(NewBufferPool(), drpcmetrics.ConnectionMetrics{})
	rb.setMaxBytes(1 << 20)

	rb.Enqueue([]byte("abc"))
	assert.Equal(t, rb.bytes, int64(3))

	_, err := rb.Dequeue()
	assert.NoError(t, err)
	assert.Equal(t, rb.bytes, int64(0)) // released on dispatch, before Done
	rb.Done()
	assert.Equal(t, rb.bytes, int64(0))
}

// A single message larger than the whole budget is still accepted on an empty
// queue (a started message must be deliverable), but the next enqueue then
// blocks.
func TestRingBuffer_ByteBudgetAcceptsOversizedOnEmpty(t *testing.T) {
	var rb ringBuffer
	rb.init(NewBufferPool(), drpcmetrics.ConnectionMetrics{})
	rb.setMaxBytes(4)

	assert.That(t, rb.Enqueue([]byte("0123456789"))) // 10 > budget, empty -> accepted
	assert.Equal(t, rb.bytes, int64(10))

	assert.That(t, !rb.Enqueue([]byte("x"))) // over budget -> rejected

	data, err := rb.Dequeue()
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("0123456789"))

	assert.That(t, rb.Enqueue([]byte("x"))) // empty again -> accepted
}

// Under a byte budget the ring grows past its initial slot count instead of
// blocking on slots, preserving FIFO order.
func TestRingBuffer_ByteBudgetGrowsRing(t *testing.T) {
	var rb ringBuffer
	rb.init(NewBufferPool(), drpcmetrics.ConnectionMetrics{})
	rb.buf = make([]*[]byte, 2) // tiny initial slot capacity
	rb.setMaxBytes(1 << 20)     // generous byte budget

	const n = 10 // far exceeds the 2 initial slots
	for i := 0; i < n; i++ {
		rb.Enqueue([]byte{byte(i)})
	}
	assert.That(t, len(rb.buf) >= n) // grew to hold all messages

	for i := 0; i < n; i++ {
		data, err := rb.Dequeue()
		assert.NoError(t, err)
		assert.Equal(t, data[0], byte(i))
	}
}

func TestRingBuffer_WithPool(t *testing.T) {
	pool := NewBufferPool()
	var rb ringBuffer
	rb.init(pool, drpcmetrics.ConnectionMetrics{})

	rb.Enqueue([]byte("pooled"))

	data, err := rb.Dequeue()
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("pooled"))
	rb.Done()

	rb.Close(io.EOF)
}
