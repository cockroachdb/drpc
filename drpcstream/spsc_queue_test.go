// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcstream

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zeebo/assert"
)

func TestSPSCQueue_BasicEnqueueDequeue(t *testing.T) {
	q := newSPSCQueue(4)
	q.Enqueue([]byte("hello"))
	data, err := q.Dequeue()
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("hello"))
	q.Done()
}

func TestSPSCQueue_FIFO(t *testing.T) {
	q := newSPSCQueue(4)
	q.Enqueue([]byte("first"))
	q.Enqueue([]byte("second"))
	q.Enqueue([]byte("third"))

	for _, want := range []string{"first", "second", "third"} {
		data, err := q.Dequeue()
		assert.NoError(t, err)
		assert.DeepEqual(t, data, []byte(want))
		q.Done()
	}
}

func TestSPSCQueue_SlotReuse(t *testing.T) {
	// Capacity 2: verify slots are recycled after Done.
	q := newSPSCQueue(2)

	for i := 0; i < 10; i++ {
		q.Enqueue([]byte("data"))
		data, err := q.Dequeue()
		assert.NoError(t, err)
		assert.DeepEqual(t, data, []byte("data"))
		q.Done()
	}
}

func TestSPSCQueue_BlockOnEmpty(t *testing.T) {
	q := newSPSCQueue(4)
	done := make(chan []byte, 1)

	go func() {
		data, err := q.Dequeue()
		assert.NoError(t, err)
		done <- append([]byte(nil), data...)
		q.Done()
	}()

	// Consumer should be blocked.
	select {
	case <-done:
		t.Fatal("Dequeue returned before Enqueue")
	case <-time.After(50 * time.Millisecond):
	}

	q.Enqueue([]byte("arrived"))
	select {
	case data := <-done:
		assert.DeepEqual(t, data, []byte("arrived"))
	case <-time.After(5 * time.Second):
		t.Fatal("Dequeue did not unblock after Enqueue")
	}
}

func TestSPSCQueue_BlockOnFull(t *testing.T) {
	q := newSPSCQueue(2)
	q.Enqueue([]byte("a"))
	q.Enqueue([]byte("b"))

	enqueued := make(chan struct{})
	go func() {
		q.Enqueue([]byte("c")) // should block — queue is full
		close(enqueued)
	}()

	// Producer should be blocked.
	select {
	case <-enqueued:
		t.Fatal("Enqueue returned on full queue")
	case <-time.After(50 * time.Millisecond):
	}

	// Consume one item to free a slot.
	data, err := q.Dequeue()
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("a"))
	q.Done()

	select {
	case <-enqueued:
	case <-time.After(5 * time.Second):
		t.Fatal("Enqueue did not unblock after Done")
	}
}

func TestSPSCQueue_CloseUnblocksProducer(t *testing.T) {
	q := newSPSCQueue(2)
	q.Enqueue([]byte("a"))
	q.Enqueue([]byte("b"))

	returned := make(chan struct{})
	go func() {
		q.Enqueue([]byte("c")) // blocks — full
		close(returned)
	}()

	select {
	case <-returned:
		t.Fatal("Enqueue returned before Close")
	case <-time.After(50 * time.Millisecond):
	}

	q.Close(errors.New("closed"))

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("Enqueue did not unblock after Close")
	}
}

func TestSPSCQueue_CloseUnblocksConsumer(t *testing.T) {
	q := newSPSCQueue(4)

	returned := make(chan error, 1)
	go func() {
		_, err := q.Dequeue()
		returned <- err
	}()

	select {
	case <-returned:
		t.Fatal("Dequeue returned before Close")
	case <-time.After(50 * time.Millisecond):
	}

	closeErr := errors.New("done")
	q.Close(closeErr)

	select {
	case err := <-returned:
		assert.Equal(t, err, closeErr)
	case <-time.After(5 * time.Second):
		t.Fatal("Dequeue did not unblock after Close")
	}
}

func TestSPSCQueue_CloseDrainsPendingItems(t *testing.T) {
	q := newSPSCQueue(4)
	q.Enqueue([]byte("pending"))

	closeErr := errors.New("closed")
	q.Close(closeErr)

	// Pending items are drained before the close error is returned.
	data, err := q.Dequeue()
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("pending"))
	q.Done()

	// Now the queue is empty and closed.
	_, err = q.Dequeue()
	assert.Equal(t, err, closeErr)
}

func TestSPSCQueue_EnqueueAfterClose(t *testing.T) {
	q := newSPSCQueue(4)
	q.Close(errors.New("closed"))

	// Should be a no-op, not panic or block.
	q.Enqueue([]byte("ignored"))
}

func TestSPSCQueue_DoubleClose(t *testing.T) {
	q := newSPSCQueue(4)
	q.Close(errors.New("first"))
	q.Close(errors.New("second")) // no-op

	_, err := q.Dequeue()
	assert.Equal(t, err.Error(), "first")
}

func TestSPSCQueue_CloseWaitsForHeld(t *testing.T) {
	q := newSPSCQueue(4)
	q.Enqueue([]byte("data"))

	data, err := q.Dequeue()
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("data"))
	// data is held — Done not yet called.

	closed := make(chan struct{})
	go func() {
		q.Close(errors.New("closed"))
		close(closed)
	}()

	// Close should block because data is held.
	select {
	case <-closed:
		t.Fatal("Close returned while data is held")
	case <-time.After(50 * time.Millisecond):
	}

	q.Done()

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after Done")
	}
}

func TestSPSCQueue_ConcurrentStress(t *testing.T) {
	const numMessages = 10000
	q := newSPSCQueue(8)

	var wg sync.WaitGroup
	wg.Add(2)

	// Producer.
	go func() {
		defer wg.Done()
		for i := 0; i < numMessages; i++ {
			q.Enqueue([]byte{byte(i), byte(i >> 8)})
		}
	}()

	// Consumer: dequeue exactly numMessages items, then signal done.
	received := 0
	go func() {
		defer wg.Done()
		for i := 0; i < numMessages; i++ {
			_, err := q.Dequeue()
			assert.NoError(t, err)
			received++
			q.Done()
		}
	}()

	wg.Wait()
	assert.Equal(t, received, numMessages)
	q.Close(errors.New("done"))
}

func TestSPSCQueue_DataIsolation(t *testing.T) {
	// Verify that Enqueue copies data — modifying the source after
	// Enqueue must not affect the queued data.
	q := newSPSCQueue(4)
	src := []byte("original")
	q.Enqueue(src)
	src[0] = 'X' // mutate source

	data, err := q.Dequeue()
	assert.NoError(t, err)
	assert.DeepEqual(t, data, []byte("original"))
	q.Done()
}

func TestRoundUpPow2(t *testing.T) {
	tests := []struct {
		in, want int
	}{
		{0, 2},
		{1, 2},
		{2, 2},
		{3, 4},
		{4, 4},
		{5, 8},
		{7, 8},
		{8, 8},
		{9, 16},
		{10, 16},
		{16, 16},
		{17, 32},
	}
	for _, tt := range tests {
		got := roundUpPow2(tt.in)
		assert.Equal(t, got, tt.want)
	}
}
