// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import "sync"

// defaultPacketQueueCapacity is the number of messages the packet queue can
// hold before the producer blocks. This decouples the transport reader
// (manageReader) from the consumer (RPC handler), preventing a slow handler
// from blocking frame delivery to other streams.
//
// TODO: benchmark whether power-of-2 masking improves performance over modulo.
const defaultPacketQueueCapacity = 256

// packetQueue is a bounded single-producer / single-consumer queue for
// assembled packet data. It sits between manageReader (producer, calls Put)
// and the application goroutine (consumer, calls Get/Done).
//
// It is implemented as a ring buffer with mutex + cond synchronization.
// Slots are pre-allocated and reused: each slot's backing array grows via
// append to fit incoming data, then stays at its high-water mark, avoiding
// per-message allocation in steady state.
//
// After Close, Get drains any queued messages before returning the close
// error. This ensures graceful shutdown (KindClose/KindCloseSend) delivers
// all buffered data to the consumer.
type packetQueue struct {
	mu   sync.Mutex
	cond sync.Cond

	buf  [][]byte // ring buffer of byte slices
	head int      // next write position (producer)
	tail int      // next read position (consumer)
	count int     // number of occupied slots

	held bool  // true between Get and Done
	err  error // terminal error, set by Close
}

func (pq *packetQueue) init() {
	pq.cond.L = &pq.mu
	pq.buf = make([][]byte, defaultPacketQueueCapacity)
}

// Put copies data into the next write slot. If the queue is full, it blocks
// until a slot is freed or the queue is closed. If the queue is closed, Put
// returns silently without enqueuing.
func (pq *packetQueue) Put(data []byte) {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	for pq.count == len(pq.buf) && pq.err == nil {
		pq.cond.Wait()
	}
	if pq.err != nil {
		return
	}

	pq.buf[pq.head] = append(pq.buf[pq.head][:0], data...)
	pq.head = (pq.head + 1) % len(pq.buf)
	pq.count++
	pq.cond.Broadcast()
}

// Get returns the data from the next read slot. If the queue is empty, it
// blocks until data is available or the queue is closed. The returned slice
// is valid until Done is called.
func (pq *packetQueue) Get() ([]byte, error) {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	for pq.count == 0 && pq.err == nil {
		pq.cond.Wait()
	}
	if pq.count == 0 {
		// Queue is empty and closed — return the close error.
		return nil, pq.err
	}

	// Return data even if closed, draining pending items first.
	pq.held = true
	return pq.buf[pq.tail], nil
}

// Done advances the read pointer, making the slot available for reuse.
// It must be called exactly once after each successful Get.
func (pq *packetQueue) Done() {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	pq.tail = (pq.tail + 1) % len(pq.buf)
	pq.count--
	pq.held = false
	pq.cond.Broadcast()
}

// Close marks the queue as closed with the given error. All blocked Put and
// Get calls are woken and will return. Close waits for any in-progress
// Get/Done pair to complete before setting the error. Subsequent calls are
// no-ops.
func (pq *packetQueue) Close(err error) {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	for pq.held {
		pq.cond.Wait()
	}
	if pq.err != nil {
		return
	}

	pq.err = err
	pq.cond.Broadcast()
}
