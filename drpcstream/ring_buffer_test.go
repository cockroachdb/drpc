// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import (
	"io"
	"sync"
	"testing"

	"github.com/zeebo/assert"
)

func TestRingBuffer_EnqueueDequeue(t *testing.T) {
	var rb ringBuffer
	rb.init(NewBufferPool(), ringBufferHooks{})

	rb.Enqueue([]byte("hello"))

	data, err := rb.Dequeue()
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("hello"))
}

func TestRingBuffer_FIFO(t *testing.T) {
	var rb ringBuffer
	rb.init(NewBufferPool(), ringBufferHooks{})

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
	rb.init(NewBufferPool(), ringBufferHooks{})

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
	rb.cond.L = &rb.mu
	rb.pool = NewBufferPool()
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
	rb.init(NewBufferPool(), ringBufferHooks{})

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
	rb.cond.L = &rb.mu
	rb.pool = NewBufferPool()
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
	rb.init(NewBufferPool(), ringBufferHooks{})

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
	rb.init(NewBufferPool(), ringBufferHooks{})

	rb.Close(io.EOF)
	rb.Close(io.ErrUnexpectedEOF) // should not overwrite

	_, err := rb.Dequeue()
	assert.Equal(t, err, io.EOF) // original error preserved
}

func TestRingBuffer_EnqueueAfterClose(t *testing.T) {
	var rb ringBuffer
	rb.init(NewBufferPool(), ringBufferHooks{})

	rb.Close(io.EOF)
	rb.Enqueue([]byte("dropped")) // should not panic or block
}

func TestRingBuffer_SlotReuse(t *testing.T) {
	var rb ringBuffer
	rb.cond.L = &rb.mu
	rb.pool = NewBufferPool()
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
	rb.init(NewBufferPool(), ringBufferHooks{})

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

func TestRingBuffer_WithPool(t *testing.T) {
	pool := NewBufferPool()
	var rb ringBuffer
	rb.init(pool, ringBufferHooks{})

	rb.Enqueue([]byte("pooled"))

	data, err := rb.Dequeue()
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("pooled"))
	rb.Done()

	rb.Close(io.EOF)
}
