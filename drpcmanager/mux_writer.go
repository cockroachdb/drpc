// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcmanager

import (
	"sync"

	"storj.io/drpc/drpcwire"
)

// muxWriter implements drpcwire.StreamWriter by serializing frame bytes into a
// shared write buffer. The manageWriter goroutine drains the buffer and writes
// directly to the transport.
//
// Compared to the previous frameQueue approach, this avoids:
//   - copying frame payload into an intermediate queue slot,
//   - drpcwire.Writer mutex overhead in the writer goroutine.
//
// Frames are serialized (via AppendFrame) into the shared buffer under a
// short-held mutex. The frame's Data slice is consumed before WriteFrame
// returns, so callers may safely reuse their buffers afterward.
type muxWriter struct {
	sw *sharedWriteBuf
}

func (w *muxWriter) WriteFrame(fr drpcwire.Frame) error {
	return w.sw.Append(fr)
}

// Flush is a no-op because the manageWriter goroutine flushes to the
// transport after draining the shared buffer.
func (w *muxWriter) Flush() error { return nil }

func (w *muxWriter) Empty() bool { return true }

// sharedWriteBuf collects serialized frame bytes from multiple concurrent
// producers. A single consumer (manageWriter) drains the buffer and writes
// the pre-serialized bytes to the transport.
type sharedWriteBuf struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
}

func newSharedWriteBuf() *sharedWriteBuf {
	sw := &sharedWriteBuf{}
	sw.cond = sync.NewCond(&sw.mu)
	return sw
}

// Append serializes fr into the shared buffer. The frame's Data slice is
// consumed (copied by AppendFrame) before Append returns.
func (sw *sharedWriteBuf) Append(fr drpcwire.Frame) error {
	sw.mu.Lock()
	if sw.closed {
		sw.mu.Unlock()
		return managerClosed.New("enqueue")
	}
	sw.buf = drpcwire.AppendFrame(sw.buf, fr)
	sw.mu.Unlock()

	sw.cond.Signal()
	return nil
}

// Drain swaps out accumulated bytes, giving the caller ownership of the
// returned slice. The internal buffer is replaced with spare (reset to zero
// length) so producers can continue appending without allocation.
func (sw *sharedWriteBuf) Drain(spare []byte) []byte {
	sw.mu.Lock()
	data := sw.buf
	sw.buf = spare
	sw.mu.Unlock()
	return data
}

// WaitAndDrain blocks until data is available or the buffer is closed.
// Returns the accumulated bytes and true if data was available, or nil and
// false if the buffer is closed and empty.
func (sw *sharedWriteBuf) WaitAndDrain(spare []byte) ([]byte, bool) {
	sw.mu.Lock()
	for len(sw.buf) == 0 && !sw.closed {
		sw.cond.Wait()
	}
	if sw.closed && len(sw.buf) == 0 {
		sw.mu.Unlock()
		return nil, false
	}
	data := sw.buf
	sw.buf = spare
	sw.mu.Unlock()
	return data, true
}

// Close marks the buffer as closed and wakes the consumer.
func (sw *sharedWriteBuf) Close() {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	if sw.closed {
		return
	}
	sw.closed = true
	sw.cond.Broadcast()
}
