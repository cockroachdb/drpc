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
// Enqueue) and the application goroutine (consumer, calls Dequeue).
//
// Buffers are obtained from a shared BufferPool. Enqueue copies data into a
// pooled buffer; Dequeue returns ownership of that buffer to the caller and
// advances the tail immediately. The caller is responsible for returning the
// buffer to the pool via BufferPool.Put.
//
// After Close, Dequeue drains any queued messages before returning the close
// error. This ensures graceful shutdown (KindClose/KindCloseSend) delivers
// all buffered data to the consumer.
type ringBuffer struct {
	mu   sync.Mutex
	cond sync.Cond

	pool  *BufferPool // shared pool; nil means allocate fresh each time
	buf   []*[]byte   // ring of pooled buffer pointers
	head  int         // next write position (producer)
	tail  int         // next read position (consumer)
	count int         // number of occupied slots

	err error // terminal error, set by Close
}

func (rb *ringBuffer) init(pool *BufferPool) {
	rb.cond.L = &rb.mu
	rb.pool = pool
	rb.buf = make([]*[]byte, defaultRingBufferCapacity)
}

// Enqueue copies data into a pooled buffer and places it in the next write
// slot. It only signals the consumer when the buffer transitions from empty
// to non-empty; subsequent enqueues into a non-empty buffer skip the signal
// since the consumer is already awake or will find data when it next checks.
func (rb *ringBuffer) Enqueue(data []byte) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	for rb.count == len(rb.buf) && rb.err == nil {
		rb.cond.Wait()
	}
	if rb.err != nil {
		return
	}

	b := rb.pool.Get()
	*b = append(*b, data...)

	rb.buf[rb.head] = b
	rb.head = (rb.head + 1) % len(rb.buf)
	wasEmpty := rb.count == 0
	rb.count++

	if wasEmpty {
		rb.cond.Signal()
	}
}

// Dequeue returns the next buffered message. The returned *[]byte is owned
// by the caller; the tail is advanced immediately. If the ring buffer has a
// pool, the caller should return the buffer via BufferPool.Put when done.
func (rb *ringBuffer) Dequeue() (*[]byte, error) {
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
	rb.cond.Signal()

	return b, nil
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
