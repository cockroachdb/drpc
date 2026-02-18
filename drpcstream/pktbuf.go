// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcstream

import (
	"io"
	"sync"
)

// pktBufPool recycles byte slices used on the read path to avoid per-packet
// allocations. Buffers flow: AcquirePacketBuf → reader → Put (zero-copy
// ownership transfer) → Get → consumer → recycle → pool.
var pktBufPool = sync.Pool{}

// AcquirePacketBuf returns a byte slice from the shared packet buffer pool
// for use as a read buffer. Returns nil if the pool is empty.
func AcquirePacketBuf() []byte {
	if v := pktBufPool.Get(); v != nil {
		return v.([]byte)[:0]
	}
	return nil
}

type packetBuffer struct {
	mu   sync.Mutex
	cond sync.Cond
	err  error
	data [][]byte
}

func (pb *packetBuffer) init() {
	pb.cond.L = &pb.mu
}

func (pb *packetBuffer) Close(err error) {
	pb.mu.Lock()
	defer pb.mu.Unlock()

	if pb.err == nil {
		// Preserve already-queued messages on graceful close so readers can
		// drain them before seeing EOF.
		if err != io.EOF {
			for i := range pb.data {
				pktBufPool.Put(pb.data[i][:0])
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
func (pb *packetBuffer) Put(data []byte) {
	pb.mu.Lock()
	defer pb.mu.Unlock()

	if pb.err != nil {
		pktBufPool.Put(data[:0])
		return
	}

	pb.data = append(pb.data, data)
	pb.cond.Broadcast()
}

func (pb *packetBuffer) Get() ([]byte, error) {
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

// recycle returns a buffer obtained from Get back to the pool.
// Call this after the data has been fully consumed (e.g. after Unmarshal).
func (pb *packetBuffer) recycle(buf []byte) {
	if buf != nil {
		pktBufPool.Put(buf[:0])
	}
}

func (pb *packetBuffer) Done() {
	// Kept for backward compatibility with stream callers.
}
