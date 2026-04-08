// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import (
	"io"
	"sync"
	"testing"

	"github.com/zeebo/assert"
)

func TestPacketQueue_PutGet(t *testing.T) {
	var pq packetQueue
	pq.init()

	pq.Put([]byte("hello"))

	data, err := pq.Get()
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("hello"))
	pq.Done()
}

func TestPacketQueue_FIFO(t *testing.T) {
	var pq packetQueue
	pq.init()

	pq.Put([]byte("first"))
	pq.Put([]byte("second"))
	pq.Put([]byte("third"))

	for _, want := range []string{"first", "second", "third"} {
		data, err := pq.Get()
		assert.NoError(t, err)
		assert.DeepEqual(t, data, []byte(want))
		pq.Done()
	}
}

func TestPacketQueue_GetBlocksUntilPut(t *testing.T) {
	var pq packetQueue
	pq.init()

	got := make(chan []byte, 1)
	go func() {
		data, err := pq.Get()
		assert.NoError(t, err)
		got <- data
	}()

	pq.Put([]byte("delayed"))
	assert.DeepEqual(t, <-got, []byte("delayed"))
	pq.Done()
}

func TestPacketQueue_PutBlocksWhenFull(t *testing.T) {
	var pq packetQueue
	pq.cond.L = &pq.mu
	pq.buf = make([][]byte, 2) // capacity 2

	pq.Put([]byte("a"))
	pq.Put([]byte("b"))

	// Third put should block until we drain one.
	done := make(chan struct{})
	go func() {
		pq.Put([]byte("c"))
		close(done)
	}()

	// Drain one slot.
	data, err := pq.Get()
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("a"))
	pq.Done()

	// Now the blocked Put should complete.
	<-done

	// Verify remaining items.
	data, err = pq.Get()
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("b"))
	pq.Done()

	data, err = pq.Get()
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("c"))
	pq.Done()
}

func TestPacketQueue_CloseUnblocksGet(t *testing.T) {
	var pq packetQueue
	pq.init()

	errch := make(chan error, 1)
	go func() {
		_, err := pq.Get()
		errch <- err
	}()

	pq.Close(io.EOF)
	assert.Equal(t, <-errch, io.EOF)
}

func TestPacketQueue_CloseUnblocksPut(t *testing.T) {
	var pq packetQueue
	pq.cond.L = &pq.mu
	pq.buf = make([][]byte, 1) // capacity 1

	pq.Put([]byte("fill"))

	done := make(chan struct{})
	go func() {
		pq.Put([]byte("blocked"))
		close(done)
	}()

	pq.Close(io.EOF)
	<-done
}

func TestPacketQueue_CloseDrainsQueued(t *testing.T) {
	var pq packetQueue
	pq.init()

	pq.Put([]byte("queued"))
	pq.Close(io.EOF)

	// Get returns the queued data first.
	data, err := pq.Get()
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("queued"))
	pq.Done()

	// Next Get returns the close error.
	data, err = pq.Get()
	assert.Nil(t, data)
	assert.Equal(t, err, io.EOF)
}

func TestPacketQueue_CloseIdempotent(t *testing.T) {
	var pq packetQueue
	pq.init()

	pq.Close(io.EOF)
	pq.Close(io.ErrUnexpectedEOF) // should not overwrite

	_, err := pq.Get()
	assert.Equal(t, err, io.EOF) // original error preserved
}

func TestPacketQueue_PutAfterClose(t *testing.T) {
	var pq packetQueue
	pq.init()

	pq.Close(io.EOF)
	pq.Put([]byte("dropped")) // should not panic or block
}

func TestPacketQueue_SlotReuse(t *testing.T) {
	var pq packetQueue
	pq.cond.L = &pq.mu
	pq.buf = make([][]byte, 2)

	// Fill and drain a few rounds to exercise slot reuse.
	for round := 0; round < 5; round++ {
		pq.Put([]byte("data"))
		data, err := pq.Get()
		assert.NoError(t, err)
		assert.DeepEqual(t, data, []byte("data"))
		pq.Done()
	}
}

func TestPacketQueue_CloseWaitsForHeld(t *testing.T) {
	var pq packetQueue
	pq.init()

	pq.Put([]byte("msg"))

	// Get the data but don't call Done yet.
	data, err := pq.Get()
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("msg"))

	closed := make(chan struct{})
	go func() {
		pq.Close(io.EOF)
		close(closed)
	}()

	// Close should be blocked because held is true.
	// Call Done to release it.
	pq.Done()
	<-closed
}

func TestPacketQueue_ConcurrentProducerConsumer(t *testing.T) {
	var pq packetQueue
	pq.init()

	const n = 1000
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			pq.Put([]byte{byte(i)})
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			data, err := pq.Get()
			assert.NoError(t, err)
			assert.Equal(t, data[0], byte(i))
			pq.Done()
		}
	}()

	wg.Wait()
	pq.Close(io.EOF)
}
