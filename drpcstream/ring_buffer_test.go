// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import (
	"io"
	"sync"
	"testing"
	"time"

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

// When a byte budget is set (flow control on), the producer blocks on buffered
// bytes rather than slot count, and a Dequeue that drops below the budget wakes
// it.
func TestRingBuffer_ByteBudgetBlocksProducer(t *testing.T) {
	var rb ringBuffer
	rb.init(NewBufferPool(), drpcmetrics.ConnectionMetrics{})
	rb.setMaxBytes(8) // room for two 4-byte messages

	rb.Enqueue([]byte("aaaa")) // bytes = 4
	rb.Enqueue([]byte("bbbb")) // bytes = 8, now at budget

	// Third enqueue must block until a dispatch frees bytes.
	done := make(chan struct{})
	go func() {
		rb.Enqueue([]byte("cccc"))
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("Enqueue returned while over byte budget")
	case <-time.After(50 * time.Millisecond):
	}

	// Dispatch one message: bytes drops to 4, below the budget.
	data, err := rb.Dequeue()
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("aaaa"))

	<-done // blocked Enqueue now completes

	for _, want := range []string{"bbbb", "cccc"} {
		data, err := rb.Dequeue()
		assert.NoError(t, err)
		assert.DeepEqual(t, data, []byte(want))
	}
}

// The incoming message's length is part of the admission check, so the queue
// never overshoots its budget by a message.
func TestRingBuffer_ByteBudgetIncludesIncomingLength(t *testing.T) {
	var rb ringBuffer
	rb.init(NewBufferPool(), drpcmetrics.ConnectionMetrics{})
	rb.setMaxBytes(8)

	rb.Enqueue([]byte("aaaaaaa")) // 7 bytes, queue empty -> accepted
	assert.Equal(t, rb.bytes, int64(7))

	// 7 + 4 = 11 > 8, so the 4-byte message must block even though the 7 bytes
	// already queued are under budget.
	done := make(chan struct{})
	go func() {
		rb.Enqueue([]byte("bbbb"))
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("Enqueue admitted a message that overshoots the budget")
	case <-time.After(50 * time.Millisecond):
	}

	data, err := rb.Dequeue() // frees 7 bytes
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("aaaaaaa"))
	<-done // now 0 + 4 <= 8, admitted
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

	rb.Enqueue([]byte("0123456789")) // 10 bytes > budget, accepted (queue empty)
	assert.Equal(t, rb.bytes, int64(10))

	done := make(chan struct{})
	go func() {
		rb.Enqueue([]byte("x"))
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("Enqueue returned while over byte budget")
	case <-time.After(50 * time.Millisecond):
	}

	data, err := rb.Dequeue()
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("0123456789"))

	<-done // now under budget, blocked Enqueue completes
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
