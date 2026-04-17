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
// Buffers are obtained from a shared BufferPool. Enqueue copies data into a
// pooled buffer; Dequeue returns that buffer's data and advances the tail
// immediately, and Done releases the buffer back to the pool. Keeping the
// pool behind Dequeue/Done means the consumer does not need to know whether
// the queue is backed by a pool or by fixed buffers.
//
// After Close, Dequeue drains any queued messages before returning the close
// error. This ensures graceful shutdown (KindClose/KindCloseSend) delivers
// all buffered data to the consumer.
type ringBuffer struct {
	mu   sync.Mutex
	cond sync.Cond

	// pool is shared across all streams on a connection and is owned by the
	// Manager, not the ring buffer. Its lifetime outlives this buffer, so a
	// consumer may safely return a buffer via Done even after Close.
	pool  *BufferPool
	buf   []*[]byte // ring of pooled buffer pointers
	head  int       // next write position (producer)
	tail  int       // next read position (consumer)
	count int       // number of occupied slots

	held *[]byte // buffer from the last Dequeue, released by Done
	err  error   // terminal error, set by Close
}

func (rb *ringBuffer) init(pool *BufferPool) {
	rb.cond.L = &rb.mu
	rb.pool = pool
	rb.buf = make([]*[]byte, defaultRingBufferCapacity)
}

// Enqueue copies data into a pooled buffer and places it in the next write
// slot. If the buffer is full, it blocks until a slot is freed or the buffer
// is closed. If the buffer is closed, Enqueue returns silently.
func (rb *ringBuffer) Enqueue(data []byte) {
	b := rb.pool.Get()
	*b = append(*b, data...)

	rb.mu.Lock()
	defer rb.mu.Unlock()

	for rb.count == len(rb.buf) && rb.err == nil {
		rb.cond.Wait()
	}
	if rb.err != nil {
		rb.pool.Put(b)
		return
	}

	rb.buf[rb.head] = b
	rb.head = (rb.head + 1) % len(rb.buf)
	rb.count++
	rb.cond.Broadcast()
}

// Dequeue returns the data from the next buffered message and advances the
// tail. The returned slice is valid until Done is called, which releases the
// underlying buffer back to the pool. Done must be called exactly once after
// each successful Dequeue.
func (rb *ringBuffer) Dequeue() ([]byte, error) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	for rb.count == 0 && rb.err == nil {
		rb.cond.Wait()
	}
	if rb.count == 0 && rb.err != nil {
		return nil, rb.err
	}

	b := rb.buf[rb.tail]
	rb.buf[rb.tail] = nil
	rb.tail = (rb.tail + 1) % len(rb.buf)
	rb.count--
	rb.held = b
	rb.cond.Broadcast()

	return *b, nil
}

// Done releases the buffer from the most recent Dequeue back to the pool,
// invalidating the slice that Dequeue returned. It must be called exactly
// once after each successful Dequeue. Because the queue is single-consumer,
// Done is only ever called from the same goroutine as Dequeue.
func (rb *ringBuffer) Done() {
	rb.pool.Put(rb.held)
	rb.held = nil
}

// Close marks the buffer as closed with the given error. All blocked Enqueue
// and Dequeue calls are woken and will return. Subsequent calls are no-ops.
func (rb *ringBuffer) Close(err error) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if rb.err != nil {
		return
	}

	rb.err = err
	rb.cond.Broadcast()
}
