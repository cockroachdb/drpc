// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import (
	"sync"

	"storj.io/drpc/drpcmetrics"
)

// defaultRingBufferCapacity is the initial number of message slots. When no
// byte budget is set (flow control off) it is also the fixed bound at which the
// producer blocks; under a byte budget the ring grows past it. Either way it
// decouples the transport reader (manageReader) from the consumer (RPC handler),
// preventing a slow handler from blocking frame delivery to other streams.
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
// The producer is bound either by a payload-byte budget (setMaxBytes,
// installed with flow control) or, when no budget is set, by a fixed slot count.
// Under a byte budget the ring grows to hold more messages and blocks a message
// whose bytes would exceed the budget. The budget is in payload bytes -- the
// same unit the sender debits and grants return -- so a conforming sender never
// blocks the shared reader.
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
	bytes int64     // buffered data bytes (sum of queued message lengths)

	// maxBytes is the byte budget installed with flow control.
	maxBytes int64

	held *[]byte // buffer from the last Dequeue, released by Done
	err  error   // terminal error, set by Close

	metrics drpcmetrics.ConnectionMetrics
}

func (rb *ringBuffer) init(pool *BufferPool, metrics drpcmetrics.ConnectionMetrics) {
	rb.cond.L = &rb.mu
	rb.pool = pool
	rb.buf = make([]*[]byte, defaultRingBufferCapacity)
	rb.metrics = metrics.WithDefaults()
}

// setMaxBytes installs the payload-byte budget that bounds the producer when
// flow control is enabled. It must be called during stream construction, before
// any Enqueue.
func (rb *ringBuffer) setMaxBytes(n int64) {
	rb.maxBytes = n
}

// admits reports whether a message of n data bytes can be enqueued now. Under a
// byte budget, the queued bytes plus this message must fit. An empty queue always
// accepts one message, so an oversized message is still delivered. Without a
// budget the bound is the fixed slot count.
func (rb *ringBuffer) admits(n int64) bool {
	if rb.maxBytes <= 0 {
		return rb.count < len(rb.buf)
	}
	if rb.count == 0 {
		return true
	}
	return rb.bytes+n <= rb.maxBytes
}

// grow doubles the ring's slot capacity, re-linearizing the occupied slots from
// tail. It is only reached under a byte budget, where slot count no longer
// bounds the producer.
func (rb *ringBuffer) grow() {
	newBuf := make([]*[]byte, 2*len(rb.buf))
	for i := 0; i < rb.count; i++ {
		newBuf[i] = rb.buf[(rb.tail+i)%len(rb.buf)]
	}
	rb.buf = newBuf
	rb.head = rb.count
	rb.tail = 0
}

// Enqueue copies data into a pooled buffer in the next write slot and reports
// whether it was admitted. Under a byte budget it does not block: a message that
// would exceed the budget returns false, signalling a receive-cap overrun by a
// non-compliant sender that the caller must fail-stop -- so the shared reader is
// never blocked. Without a budget it blocks until a slot frees (legacy). A closed
// buffer drops the message and returns true (nothing to fail-stop).
func (rb *ringBuffer) Enqueue(data []byte) (admitted bool) {
	n := int64(len(data))

	rb.mu.Lock()
	defer rb.mu.Unlock()

	if rb.maxBytes <= 0 {
		// Legacy slot mode: block until a slot frees or the buffer closes.
		for !rb.admits(n) && rb.err == nil {
			rb.cond.Wait()
		}
	}
	if rb.err != nil {
		return true // closed: drop, not an overrun
	}
	if !rb.admits(n) {
		return false // byte-budget overrun (non-compliant sender)
	}

	b := rb.pool.Get()
	*b = append(*b, data...)

	if rb.count == len(rb.buf) {
		rb.grow() // only reached under a byte budget; slot mode blocks above
	}

	rb.buf[rb.head] = b
	rb.head = (rb.head + 1) % len(rb.buf)
	rb.count++
	rb.bytes += n
	if rb.metrics.ShouldRecord() {
		rb.metrics.ReceiveQueueMessages.Inc(1)
		rb.metrics.ReceiveQueueBytes.Inc(n)
	}
	rb.cond.Broadcast()
	return true
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
	rb.bytes -= int64(len(*b))
	rb.held = b
	if rb.metrics.ShouldRecord() {
		rb.metrics.ReceiveQueueMessages.Inc(-1)
		rb.metrics.ReceiveQueueBytes.Inc(-int64(len(*b)))
	}
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
