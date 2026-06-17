// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcwire

import (
	"io"
	"sync"

	"github.com/zeebo/errs"
)

// defaultBufferCapacity is the initial capacity of the pending and in-flight
// write buffers.
var defaultBufferCapacity = 4096

// errInterrupted is returned by WriteFrame when its cancel channel fires while
// it is blocked on backpressure. The caller maps it to the appropriate stream
// error (e.g. via CheckCancelError).
var errInterrupted = errs.New("sending frames interrupted")

// WriterOptions controls configuration settings for a MuxWriter.
type WriterOptions struct {
	// MaximumBufferSize is the high-water mark for the pending write buffer.
	// Once it is reached, WriteFrame blocks the calling stream until the buffer
	// drains onto the wire. This bounds sender-side memory so a fast producer
	// cannot outrun a slow connection and OOM the process. Peak memory is up to
	// ~2x this value: the pending buffer plus the in-flight buffer being
	// written. When 0, it defaults to 1 MiB.
	MaximumBufferSize int
}

// MuxWriter serializes frames from many concurrent streams onto a single
// io.Writer. Callers append frames with WriteFrame; a dedicated run goroutine
// flushes them to the wire, double-buffering so producers can keep appending
// while a write is in flight.
//
// The pending buffer is bounded at maxBuf. When it is full, WriteFrame blocks
// until run frees space, the caller cancels, or the writer is stopped. This is
// the connection-level backpressure that keeps memory bounded.
type MuxWriter struct {
	w       io.Writer
	onError func(error)
	maxBuf  int // high-water mark for buf; producers block at or above it

	mu       sync.Mutex
	cond     *sync.Cond    // signaled by WriteFrame when buf becomes non-empty; awaited by run
	buf      []byte        // pending bytes awaiting flush
	closed   bool          // set once; no further writes are accepted
	closeErr error         // terminal error, returned by WriteFrame after close
	blocked  int           // producers currently parked on drain
	drain    chan struct{} // closed+replaced by run after a flush to wake parked producers
	done     chan struct{} // closed when run exits
}

// NewMuxWriter constructs a MuxWriter with default options.
func NewMuxWriter(w io.Writer, onError func(error)) *MuxWriter {
	return NewMuxWriterWithOptions(w, onError, WriterOptions{})
}

// NewMuxWriterWithOptions constructs a MuxWriter using the provided options to
// manage buffering.
func NewMuxWriterWithOptions(w io.Writer, onError func(error), opts WriterOptions) *MuxWriter {
	if opts.MaximumBufferSize == 0 {
		opts.MaximumBufferSize = 1 << 20 // Default to 1 MiB.
	}

	mw := &MuxWriter{
		w:       w,
		onError: onError,
		maxBuf:  opts.MaximumBufferSize,
		buf:     make([]byte, 0, defaultBufferCapacity),
		done:    make(chan struct{}),
		drain:   make(chan struct{}),
	}
	mw.cond = sync.NewCond(&mw.mu)
	go mw.run()
	return mw
}

// unblockWritesLocked wakes every producer parked on backpressure by closing
// the current drain channel (a broadcast) and installing a fresh one. The
// blocked guard keeps this allocation-free on the common path where nobody is
// waiting. It must be called with mu held.
func (mw *MuxWriter) unblockWritesLocked() {
	if mw.blocked > 0 {
		close(mw.drain)
		mw.drain = make(chan struct{})
	}
}

// run is the single writer goroutine. It waits for pending bytes, swaps the
// pending buffer out for an empty spare, wakes parked producers, and flushes
// the swapped-out bytes to the wire. On a write error it closes the writer and
// reports via onError.
func (mw *MuxWriter) run() {
	defer close(mw.done)
	spare := make([]byte, 0, defaultBufferCapacity)
	for {
		mw.mu.Lock()
		for len(mw.buf) == 0 && !mw.closed {
			mw.cond.Wait()
		}

		if mw.closed {
			mw.mu.Unlock()
			return
		}

		// Swap the full pending buffer for the empty spare so producers can
		// refill buf (now free) while we write the swapped-out bytes below.
		mw.buf, spare = spare, mw.buf
		mw.unblockWritesLocked()
		mw.mu.Unlock()

		if _, err := mw.w.Write(spare); err != nil {
			mw.mu.Lock()
			if mw.closed {
				mw.mu.Unlock()
				return
			}
			mw.closed = true
			mw.closeErr = err
			mw.unblockWritesLocked()
			mw.mu.Unlock()
			if mw.onError != nil {
				mw.onError(err)
			}
			return
		}

		spare = spare[:0]
	}
}

// WriteFrame appends fr to the pending buffer. If the buffer is at its
// high-water mark it blocks until run frees space, cancel fires, or the writer
// is stopped. cancel is the caller's termination channel (e.g. a stream's term
// signal); when it fires WriteFrame returns errInterrupted.
func (mw *MuxWriter) WriteFrame(fr Frame, cancel <-chan struct{}) (err error) {
	for {
		mw.mu.Lock()
		if mw.closed {
			mw.mu.Unlock()
			return mw.closeErr
		}
		if len(mw.buf) < mw.maxBuf {
			mw.buf = AppendFrame(mw.buf, fr)
			mw.cond.Signal()
			mw.mu.Unlock()
			return nil
		}

		// Buffer is full. Snapshot the drain channel under the lock before
		// parking: run may close+replace it the instant we unlock, and selecting
		// on the field instead of the snapshot would miss that wakeup.
		ch := mw.drain
		mw.blocked++
		mw.mu.Unlock()

		select {
		case <-ch:
			// Space may be available now; loop and re-check.
			mw.mu.Lock()
			mw.blocked--
			mw.mu.Unlock()
		case <-cancel:
			mw.mu.Lock()
			mw.blocked--
			mw.mu.Unlock()
			return errInterrupted
		case <-mw.done:
			mw.mu.Lock()
			mw.blocked--
			err := mw.closeErr // can be nil
			mw.mu.Unlock()
			return err
		}
	}
}

// Stop closes the writer with err. Blocked and subsequent WriteFrame calls
// return err. Parked producers are woken so they do not wait on run, which may
// be stuck in a slow underlying write.
func (mw *MuxWriter) Stop(err error) {
	mw.mu.Lock()
	if !mw.closed {
		mw.closed = true
		mw.closeErr = err
		mw.cond.Broadcast()
		mw.unblockWritesLocked()
	}
	mw.mu.Unlock()
}

// Done returns a channel that is closed when the run goroutine has exited.
func (mw *MuxWriter) Done() <-chan struct{} {
	return mw.done
}
