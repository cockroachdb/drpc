// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcstream

import (
	"sync"
)

// defaultPacketBufferSize is the number of messages the packet buffer can
// hold before the producer blocks. This decouples the transport reader
// from the consumer (RPC handler), preventing deadlocks when the handler
// is delayed before calling Recv.
const defaultPacketBufferSize = 10

// spscQueue is a bounded single-producer / single-consumer queue for byte
// slices. It is implemented as a ring buffer with mutex+cond synchronization.
//
// The producer calls Enqueue to copy data into the next write slot. If the
// queue is full, Enqueue blocks until a slot is freed or the queue is closed.
//
// The consumer calls Dequeue to get a reference to the next read slot. The
// returned slice is valid until Done is called. Done advances the read
// pointer and recycles the slot for reuse by the producer.
//
// Close sets an error and wakes all blocked waiters. After Close, Enqueue
// is a no-op and Dequeue returns the close error.
//
// Slots are pre-allocated and reused. Each slot's backing array grows via
// append to fit incoming data, then stays at its high-water mark, avoiding
// per-message allocation in steady state.
type spscQueue struct {
	mu   sync.Mutex
	cond sync.Cond

	slots [][]byte // ring buffer of byte slices
	mask  int      // len(slots) - 1, for fast modulo (capacity is power of 2)

	head int // next write position (producer)
	tail int // next read position (consumer)
	len  int // number of items in the queue

	held bool  // true between Dequeue and Done
	err  error // set by Close
}

// newSPSCQueue creates a new SPSC queue. The capacity is rounded up to the
// next power of 2 (minimum 2).
func newSPSCQueue(capacity int) *spscQueue {
	cap := roundUpPow2(capacity)
	q := &spscQueue{
		slots: make([][]byte, cap),
		mask:  cap - 1,
	}
	q.cond.L = &q.mu
	return q
}

// Enqueue copies data into the next write slot. If the queue is full, it
// blocks until a slot is freed or the queue is closed. If the queue is
// closed, Enqueue returns silently without enqueuing.
func (q *spscQueue) Enqueue(data []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for q.len > q.mask && q.err == nil {
		q.cond.Wait()
	}
	if q.err != nil {
		return
	}

	// Copy data into the slot, reusing the existing backing array if
	// it has enough capacity. This avoids allocation in steady state.
	q.slots[q.head&q.mask] = append(q.slots[q.head&q.mask][:0], data...)
	q.head++
	q.len++
	q.cond.Broadcast()
}

// Dequeue returns the data from the next read slot. If the queue is empty,
// it blocks until data is available or the queue is closed. The returned
// slice is valid until Done is called.
func (q *spscQueue) Dequeue() ([]byte, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for q.len == 0 && q.err == nil {
		q.cond.Wait()
	}
	if q.len == 0 {
		// Queue is empty and closed — return the close error.
		return nil, q.err
	}
	// Return data even if closed, draining pending items first.

	data := q.slots[q.tail&q.mask]
	q.held = true
	return data, nil
}

// Done advances the read pointer, making the slot available for reuse.
// It must be called exactly once after each successful Dequeue.
func (q *spscQueue) Done() {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.tail++
	q.len--
	q.held = false
	q.cond.Broadcast()
}

// Close marks the queue as closed with the given error. All blocked
// Enqueue and Dequeue calls are woken and will return. If Close has
// already been called, subsequent calls are no-ops.
func (q *spscQueue) Close(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Wait for any in-progress Dequeue/Done pair to complete so that
	// we don't race with the consumer's use of the dequeued data.
	for q.held {
		q.cond.Wait()
	}

	if q.err == nil {
		q.err = err
		q.cond.Broadcast()
	}
}

// roundUpPow2 rounds n up to the next power of 2. Minimum is 2.
func roundUpPow2(n int) int {
	if n <= 2 {
		return 2
	}
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	n |= n >> 32
	n++
	return n
}
