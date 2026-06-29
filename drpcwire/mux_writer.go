// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcwire

import (
	"io"
	"sync"

	"storj.io/drpc"
	"storj.io/drpc/drpcmetrics"
	"storj.io/drpc/drpcsignal"
)

// defaultBufferCapacity is the initial capacity of the pending and in-flight
// write buffers.
var defaultBufferCapacity = 4096

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
	metrics drpcmetrics.ConnectionMetrics
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
	return NewMuxWriterWithOptions(w, onError, drpcmetrics.ConnectionMetrics{}, WriterOptions{})
}

// NewMuxWriterWithOptions constructs a MuxWriter using the provided options to
// manage buffering and records connection write metrics through metrics.
func NewMuxWriterWithOptions(
	w io.Writer, onError func(error), metrics drpcmetrics.ConnectionMetrics, opts WriterOptions,
) *MuxWriter {
	if opts.MaximumBufferSize == 0 {
		opts.MaximumBufferSize = 1 << 20 // Default to 1 MiB.
	}

	mw := &MuxWriter{
		w:       w,
		onError: onError,
		metrics: metrics.WithDefaults(),
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
		inFlightBytes := int64(len(spare))
		if mw.metrics.ShouldRecord() {
			mw.metrics.WriteQueueBytes.Inc(-inFlightBytes)
			mw.metrics.WriteFlushInFlightBytes.Inc(inFlightBytes)
		}
		mw.unblockWritesLocked()
		mw.mu.Unlock()

		_, err := mw.w.Write(spare)
		if mw.metrics.ShouldRecord() {
			mw.metrics.WriteFlushInFlightBytes.Inc(-inFlightBytes)
		}
		if err != nil {
			// A failed write means the connection is gone. Classify it as a
			// ConnectionError at the source, symmetric with the read path (see
			// drpcwire.Reader.read). This wrapped error flows out both via
			// closeErr (returned by WriteFrame) and onError (manager teardown).
			err = drpc.ConnectionError.Wrap(err)
			mw.mu.Lock()
			if mw.closed {
				mw.mu.Unlock()
				return
			}
			mw.closed = true
			mw.closeErr = err
			pendingBytes := int64(len(mw.buf))
			mw.buf = mw.buf[:0]
			if mw.metrics.ShouldRecord() {
				mw.metrics.WriteQueueBytes.Inc(-pendingBytes)
			}
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
// is stopped. cancel is the caller's termination signal (e.g. a stream's send
// signal); when it fires WriteFrame stops waiting and returns cancel.Err(), so
// the caller gets the termination cause directly without interpreting a
// sentinel. cancel may be nil to wait indefinitely for space. A control-bit
// frame is appended immediately even past the high-water mark, so an abortive
// cancel is never delayed by backpressure.
func (mw *MuxWriter) WriteFrame(fr Frame, cancel *drpcsignal.Signal) (err error) {
	for {
		mw.mu.Lock()
		if mw.closed {
			mw.mu.Unlock()
			return mw.closeErr
		}
		// A control-bit frame (e.g. an abortive KindCancel) is appended even past
		// the cap so it is never delayed by backpressure. This overshoots the
		// high-water mark by at most one small control frame per terminating
		// stream, which is bounded and acceptable.
		if len(mw.buf) < mw.maxBuf || fr.Control {
			before := len(mw.buf)
			mw.buf = AppendFrame(mw.buf, fr)
			if mw.metrics.ShouldRecord() {
				mw.metrics.WriteQueueBytes.Inc(int64(len(mw.buf) - before))
			}
			mw.cond.Signal()
			mw.mu.Unlock()
			return nil
		}

		// Buffer is full. Snapshot the drain channel under the lock before
		// parking: run may close+replace it the instant we unlock, and selecting
		// on the field instead of the snapshot would miss that wakeup.
		ch := mw.drain
		mw.blocked++
		if mw.metrics.ShouldRecord() {
			mw.metrics.WriteQueueBlockedWriters.Inc(1)
			mw.metrics.WriteQueueBlockCount.Inc(1)
		}
		mw.mu.Unlock()

		// Resolve the cancel channel lazily, only now that we are parking, so the
		// common non-blocking path never forces the signal's channel to be
		// allocated.
		var cancelCh <-chan struct{}
		if cancel != nil {
			cancelCh = cancel.Signal()
		}

		select {
		case <-ch:
			// Space may be available now; loop and re-check.
			mw.mu.Lock()
			mw.blocked--
			if mw.metrics.ShouldRecord() {
				mw.metrics.WriteQueueBlockedWriters.Inc(-1)
			}
			mw.mu.Unlock()
		case <-cancelCh:
			mw.mu.Lock()
			mw.blocked--
			if mw.metrics.ShouldRecord() {
				mw.metrics.WriteQueueBlockedWriters.Inc(-1)
			}
			mw.mu.Unlock()
			return cancel.Err()
		case <-mw.done:
			mw.mu.Lock()
			mw.blocked--
			if mw.metrics.ShouldRecord() {
				mw.metrics.WriteQueueBlockedWriters.Inc(-1)
			}
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
		pendingBytes := int64(len(mw.buf))
		mw.buf = mw.buf[:0]
		if mw.metrics.ShouldRecord() {
			mw.metrics.WriteQueueBytes.Inc(-pendingBytes)
		}
		mw.cond.Broadcast()
		mw.unblockWritesLocked()
	}
	mw.mu.Unlock()
}

// Done returns a channel that is closed when the run goroutine has exited.
func (mw *MuxWriter) Done() <-chan struct{} {
	return mw.done
}
