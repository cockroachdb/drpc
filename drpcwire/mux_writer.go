// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcwire

import (
	"io"
	"sync"
)

// MuxWriter serializes Frame writes onto a single io.Writer from many
// concurrent producers. The API is split into two operations:
//
//   - Enqueue appends a frame to the pending buffer (cheap, sub-µs).
//   - Flush opportunistically becomes the leader and drains the buffer
//     by calling the underlying io.Writer.
//
// Callers must arrange for someone to call Flush after Enqueue. The
// expected pattern is `defer mw.Flush()` at every top-level write op,
// with the Enqueue calls happening under whatever lock the caller uses
// to serialize multi-frame messages.
//
// Splitting the API this way lets the caller release stream-level locks
// before the (potentially blocking) transport.Write happens. Under no
// contention, the calling goroutine writes inline as the leader, avoiding
// a thread hop. Under contention, Flush from a follower is a no-op: the
// current leader's drain loop sweeps up frames queued before its
// `len(buf) == 0` exit check, so no data is left stranded.
type MuxWriter struct {
	w        io.Writer
	mu       sync.Mutex
	cond     *sync.Cond
	buf      []byte // pending frames waiting for a writer
	spare    []byte // recycled buffer; swapped with buf during writes
	writing  bool   // a leader is currently in transport.Write
	closed   bool
	closeErr error
	onError  func(error)
	done     chan struct{}
}

var defaultBufferCapacity = 4096

func NewMuxWriter(w io.Writer, onError func(error)) *MuxWriter {
	mw := &MuxWriter{
		w:       w,
		buf:     make([]byte, 0, defaultBufferCapacity),
		spare:   make([]byte, 0, defaultBufferCapacity),
		onError: onError,
		done:    make(chan struct{}),
	}
	mw.cond = sync.NewCond(&mw.mu)
	go mw.run()
	return mw
}

// run is a coordinator goroutine that closes mw.done when the writer is
// stopped. It does not perform any writes; draining is driven entirely
// by Flush calls from the producers.
func (mw *MuxWriter) run() {
	defer close(mw.done)
	mw.mu.Lock()
	for !mw.closed {
		mw.cond.Wait()
	}
	mw.mu.Unlock()
}

// Enqueue appends fr to the pending buffer. It returns immediately
// without performing any I/O. The caller must arrange for Flush to be
// called subsequently — typically via `defer mw.Flush()` at the top of
// the calling op.
func (mw *MuxWriter) Enqueue(fr Frame) error {
	mw.mu.Lock()
	defer mw.mu.Unlock()
	if mw.closed {
		return mw.closeErr
	}
	mw.buf = AppendFrame(mw.buf, fr)
	return nil
}

// Flush opportunistically becomes the leader and drains the pending
// buffer to the underlying writer. If another goroutine is already the
// leader, Flush returns immediately; the current leader will pick up
// frames queued before its `len(buf) == 0` exit check.
func (mw *MuxWriter) Flush() error {
	mw.mu.Lock()
	if mw.closed {
		err := mw.closeErr
		mw.mu.Unlock()
		return err
	}
	if mw.writing || len(mw.buf) == 0 {
		mw.mu.Unlock()
		return nil
	}
	mw.writing = true

	for {
		data := mw.buf
		mw.buf = mw.spare[:0]
		mw.mu.Unlock()
		_, err := mw.w.Write(data)
		mw.mu.Lock()
		mw.spare = data[:0]
		if err != nil {
			alreadyClosed := mw.closed
			if !alreadyClosed {
				mw.closed = true
				mw.closeErr = err
				mw.cond.Broadcast()
			}
			mw.writing = false
			mw.mu.Unlock()
			if !alreadyClosed && mw.onError != nil {
				mw.onError(err)
			}
			return err
		}
		if mw.closed || len(mw.buf) == 0 {
			mw.writing = false
			mw.mu.Unlock()
			return nil
		}
	}
}

// WriteFrame is a convenience that performs Enqueue followed by Flush
// for callers that only need to send a single frame. Stream code should
// not use this — it must hold the stream lock across multiple Enqueue
// calls and call Flush once at the top-level op.
func (mw *MuxWriter) WriteFrame(fr Frame) error {
	if err := mw.Enqueue(fr); err != nil {
		return err
	}
	return mw.Flush()
}

// Stop marks the writer as closed with err. Buffered data is discarded
// (Stop has abort semantics). Callers waiting on Done() will be released
// after the run goroutine observes the close.
func (mw *MuxWriter) Stop(err error) {
	mw.mu.Lock()
	defer mw.mu.Unlock()
	if !mw.closed {
		mw.closed = true
		mw.closeErr = err
		mw.cond.Broadcast()
	}
}

// Done returns a channel that is closed once the writer's coordinator
// goroutine has exited following a Stop.
func (mw *MuxWriter) Done() <-chan struct{} {
	return mw.done
}
