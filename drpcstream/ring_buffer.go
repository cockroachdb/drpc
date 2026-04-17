// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import "sync"

// defaultRingBufferCapacity is the number of messages the ring buffer can
// hold before the producer blocks. This decouples the transport reader
// (manageReader) from the consumer (RPC handler), preventing a slow handler
// from blocking frame delivery to other streams.
//
// TODO: benchmark whether power-of-2 masking improves performance over modulo.
const defaultRingBufferCapacity = 256

// ringBuffer is a bounded single-producer / single-consumer FIFO queue for
// assembled packet data. It sits between manageReader (producer, calls
// Enqueue) and the application goroutine (consumer, calls Dequeue/Done).
//
// Slots are pre-allocated and reused: each slot's backing array grows via
// append to fit incoming data, then stays at its high-water mark, avoiding
// per-message allocation in steady state.
//
// After Close, Dequeue drains any queued messages before returning the close
// error. This ensures graceful shutdown (KindClose/KindCloseSend) delivers
// all buffered data to the consumer.
type ringBuffer struct {
	mu   sync.Mutex
	cond sync.Cond

	buf   [][]byte // ring of byte slices
	head  int      // next write position (producer)
	tail  int      // next read position (consumer)
	count int      // number of occupied slots

	held bool  // true between Dequeue and Done
	err  error // terminal error, set by Close
}

func (rb *ringBuffer) init() {
	rb.cond.L = &rb.mu
	rb.buf = make([][]byte, defaultRingBufferCapacity)
}

// Enqueue copies data into the next write slot. If the buffer is full, it
// blocks until a slot is freed or the buffer is closed. If the buffer is
// closed, Enqueue returns silently without enqueuing.
func (rb *ringBuffer) Enqueue(data []byte) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	for rb.count == len(rb.buf) && rb.err == nil {
		rb.cond.Wait()
	}
	if rb.err != nil {
		return
	}

	rb.buf[rb.head] = append(rb.buf[rb.head][:0], data...)
	rb.head = (rb.head + 1) % len(rb.buf)
	rb.count++
	rb.cond.Broadcast()
}

// Dequeue returns the data from the next read slot. If the buffer is empty,
// it blocks until data is available or the buffer is closed. The returned
// slice is valid until Done is called.
func (rb *ringBuffer) Dequeue() ([]byte, error) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	for rb.count == 0 && rb.err == nil {
		rb.cond.Wait()
	}
	if rb.count == 0 && rb.err != nil {
		return nil, rb.err
	}

	rb.held = true
	return rb.buf[rb.tail], nil
}

// Done advances the read pointer, making the slot available for reuse.
// It must be called exactly once after each successful Dequeue.
//
// TODO(shubham): remove this method once a shared buffer pool is introduced.
// With a pool, Dequeue will advance the tail immediately and the caller will
// return the buffer to the pool directly.
func (rb *ringBuffer) Done() {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	rb.tail = (rb.tail + 1) % len(rb.buf)
	rb.count--
	rb.held = false
	rb.cond.Broadcast()
}

// Close marks the buffer as closed with the given error. All blocked Enqueue
// and Dequeue calls are woken and will return. Close waits for any in-progress
// Dequeue/Done pair to complete before setting the error. Subsequent calls are
// no-ops.
func (rb *ringBuffer) Close(err error) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	for rb.held {
		rb.cond.Wait()
	}
	if rb.err != nil {
		return
	}

	rb.err = err
	rb.cond.Broadcast()
}
