// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcstream

import (
	"sync"
)

// packetStore is the interface for packet buffer implementations.
// syncPacketBuffer is used for non-mux mode (blocking, single-slot),
// queuePacketBuffer is used for mux mode (non-blocking, queued).
type packetStore interface {
	Put(data []byte)
	Get() ([]byte, error)
	Close(err error)
	Done()
	Recycle([]byte)
}

// syncPacketBuffer is the original single-slot, blocking packet buffer used
// in non-mux mode. Put blocks until the previous value is consumed via
// Get+Done, and the reader (manageReader) blocks in Put until the stream
// consumer finishes processing.
type syncPacketBuffer struct {
	mu   sync.Mutex
	cond sync.Cond
	err  error
	data []byte
	set  bool
	held bool
}

func (pb *syncPacketBuffer) init() {
	pb.cond.L = &pb.mu
}

func (pb *syncPacketBuffer) Close(err error) {
	pb.mu.Lock()
	defer pb.mu.Unlock()

	for pb.held {
		pb.cond.Wait()
	}

	if pb.err == nil {
		pb.data = nil
		pb.set = false
		pb.err = err
		pb.cond.Broadcast()
	}
}

func (pb *syncPacketBuffer) Put(data []byte) {
	pb.mu.Lock()
	defer pb.mu.Unlock()

	for pb.set && pb.err == nil {
		pb.cond.Wait()
	}
	if pb.err != nil {
		return
	}

	pb.data = data
	pb.set = true
	pb.held = false
	pb.cond.Broadcast()

	for pb.set || pb.held {
		pb.cond.Wait()
	}
}

func (pb *syncPacketBuffer) Get() ([]byte, error) {
	pb.mu.Lock()
	defer pb.mu.Unlock()

	for !pb.set && pb.err == nil {
		pb.cond.Wait()
	}
	if pb.err != nil {
		return nil, pb.err
	}

	pb.held = true
	pb.cond.Broadcast()

	return pb.data, nil
}

func (pb *syncPacketBuffer) Done() {
	pb.mu.Lock()
	defer pb.mu.Unlock()

	pb.data = nil
	pb.set = false
	pb.held = false
	pb.cond.Broadcast()
}

// Recycle is a no-op for syncPacketBuffer. Buffer lifetime is managed by
// the manageReader goroutine that owns the underlying slice.
func (pb *syncPacketBuffer) Recycle([]byte) {}
