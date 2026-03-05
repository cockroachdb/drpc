// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import (
	"io"
	"sync"
)

// pktBuf wraps a byte slice so the pool stores a pointer type, avoiding
// an allocation on every Put.
type pktBuf struct {
	data []byte
}

// pktBufPool recycles byte slices used on the read path to avoid per-packet
// allocations. Buffers flow: AcquirePacketBuf -> reader -> Put (zero-copy
// ownership transfer) -> Get -> consumer -> Recycle -> pool.
var pktBufPool = sync.Pool{
	New: func() interface{} { return &pktBuf{} },
}

func recycleToPktBufPool(data []byte) {
	pb := pktBufPool.Get().(*pktBuf)
	pb.data = data[:0]
	pktBufPool.Put(pb)
}

// AcquirePacketBuf returns a byte slice from the shared packet buffer pool
// for use as a read buffer. Returns nil if the pool is empty.
func AcquirePacketBuf() []byte {
	pb := pktBufPool.Get().(*pktBuf)
	data := pb.data[:0]
	pb.data = nil
	pktBufPool.Put(pb)
	return data
}

// queuePacketBuffer is a non-blocking, queue-based packet buffer used in mux
// mode. Put appends to an unbounded queue and returns immediately, allowing
// the reader goroutine to continue dispatching packets to other streams.
type queuePacketBuffer struct {
	mu   sync.Mutex
	cond sync.Cond
	err  error
	data [][]byte
}

func (pb *queuePacketBuffer) init() {
	pb.cond.L = &pb.mu
}

func (pb *queuePacketBuffer) Close(err error) {
	pb.mu.Lock()
	defer pb.mu.Unlock()

	if pb.err == nil {
		// Preserve already-queued messages on graceful close so readers can
		// drain them before seeing EOF.
		if err != io.EOF {
			for i := range pb.data {
				recycleToPktBufPool(pb.data[i])
				pb.data[i] = nil
			}
			pb.data = pb.data[:0]
		}
		pb.err = err
		pb.cond.Broadcast()
	}
}

// Put takes ownership of data. The caller must not use data after calling Put.
// If the buffer is closed, data is returned to the pool.
func (pb *queuePacketBuffer) Put(data []byte) {
	pb.mu.Lock()
	defer pb.mu.Unlock()

	if pb.err != nil {
		recycleToPktBufPool(data)
		return
	}

	pb.data = append(pb.data, data)
	pb.cond.Broadcast()
}

func (pb *queuePacketBuffer) Get() ([]byte, error) {
	pb.mu.Lock()
	defer pb.mu.Unlock()

	for len(pb.data) == 0 && pb.err == nil {
		pb.cond.Wait()
	}
	if len(pb.data) == 0 {
		return nil, pb.err
	}

	data := pb.data[0]
	n := copy(pb.data, pb.data[1:])
	pb.data[n] = nil
	pb.data = pb.data[:n]
	return data, nil
}

// Done is a no-op for queuePacketBuffer. Buffer ownership is transferred to
// the caller via Get, and recycling is done explicitly via Recycle.
func (pb *queuePacketBuffer) Done() {}

// Recycle returns a buffer obtained from Get back to the pool.
// Call this after the data has been fully consumed (e.g. after Unmarshal).
func (pb *queuePacketBuffer) Recycle(buf []byte) {
	if buf != nil {
		recycleToPktBufPool(buf)
	}
}
