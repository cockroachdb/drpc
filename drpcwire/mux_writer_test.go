// Copyright (C) 2021 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcwire

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/zeebo/assert"
)

// blockingWriter blocks in Write until unblock is closed, then returns err.
type blockingWriter struct {
	unblock chan struct{}
	err     error       // error to return once unblocked
	wrote   chan []byte // sends a copy of data on each Write entry
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{
		unblock: make(chan struct{}),
		wrote:   make(chan []byte, 10),
	}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	cp := make([]byte, len(p))
	copy(cp, p)
	w.wrote <- cp
	<-w.unblock
	if w.err != nil {
		return 0, w.err
	}
	return len(p), nil
}

// failWriter returns err on the nth call to Write (1-indexed). Calls before
// that succeed normally.
type failWriter struct {
	n     int
	count int
	err   error
	buf   bytes.Buffer
}

func newFailWriter(n int, err error) *failWriter {
	return &failWriter{n: n, err: err}
}

func (w *failWriter) Write(p []byte) (int, error) {
	w.count++
	if w.count >= w.n {
		return 0, w.err
	}
	return w.buf.Write(p)
}

func TestMuxWriter_MaxBufferSizeOption(t *testing.T) {
	// Default is applied when the option is left at zero.
	mw := NewMuxWriter(io.Discard, func(error) {})
	assert.Equal(t, mw.maxBuf, 1<<20)
	mw.Stop(nil)
	<-mw.Done()

	// An explicit value is honored.
	mw = NewMuxWriterWithOptions(io.Discard, func(error) {}, WriterOptions{MaximumBufferSize: 4096})
	assert.Equal(t, mw.maxBuf, 4096)
	mw.Stop(nil)
	<-mw.Done()
}

func TestMuxWriter(t *testing.T) {
	var exp []byte
	pr, pw := io.Pipe()
	mw := NewMuxWriter(pw, func(error) {})

	for range 1000 {
		fr := RandFrame()
		exp = AppendFrame(exp, fr)
		assert.NoError(t, mw.WriteFrame(fr, nil))
	}

	// Read exactly len(exp) bytes: this blocks until MuxWriter has drained
	// all frames through the pipe.
	got := make([]byte, len(exp))
	_, err := io.ReadFull(pr, got)
	assert.NoError(t, err)

	// Now stop the writer and close the pipe.
	mw.Stop(errors.New("stopped"))
	<-mw.Done()
	pw.Close()
	pr.Close()

	assert.That(t, bytes.Equal(exp, got))
}

func TestMuxWriter_WriteFrameAfterStop(t *testing.T) {
	mw := NewMuxWriter(io.Discard, func(error) {})
	mw.Stop(errors.New("stopped"))
	<-mw.Done()

	err := mw.WriteFrame(RandFrame(), nil)
	assert.Error(t, err)
	assert.Equal(t, err.Error(), "stopped")
}

func TestMuxWriter_ConcurrentWriteFrame(t *testing.T) {
	pr, pw := io.Pipe()
	mw := NewMuxWriter(pw, func(error) {})

	const numWriters = 10
	const framesPerWriter = 100

	// Pre-generate frames and compute total expected bytes so we can use
	// io.ReadFull to block until everything has drained (Stop has abort
	// semantics, so we can't rely on it to drain).
	allFrames := make([][]Frame, numWriters)
	var expSize int
	for i := range numWriters {
		allFrames[i] = make([]Frame, framesPerWriter)
		for j := range framesPerWriter {
			fr := Frame{
				Data: []byte{byte(j)},
				ID:   ID{Stream: uint64(i + 1), Message: uint64(j + 1)},
				Kind: KindMessage,
				Done: true,
			}
			allFrames[i][j] = fr
			expSize += len(AppendFrame(nil, fr))
		}
	}

	var wg sync.WaitGroup
	for i := range numWriters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range framesPerWriter {
				assert.NoError(t, mw.WriteFrame(allFrames[i][j], nil))
			}
		}()
	}

	wg.Wait()

	// Block until all bytes have been drained through the pipe.
	got := make([]byte, expSize)
	_, err := io.ReadFull(pr, got)
	assert.NoError(t, err)
	mw.Stop(errors.New("stopped"))
	<-mw.Done()
	pw.Close()
	pr.Close()

	// Parse received bytes and count frames.
	count := 0
	for len(got) > 0 {
		rem, _, ok, err := ParseFrame(got)
		assert.NoError(t, err)
		assert.That(t, ok)
		got = rem
		count++
	}
	assert.Equal(t, count, numWriters*framesPerWriter)
}

func TestMuxWriter_WriteErrorCallsOnError(t *testing.T) {
	writeErr := errors.New("disk full")
	fw := newFailWriter(1, writeErr)

	gotErr := make(chan error, 1)
	mw := NewMuxWriter(fw, func(err error) { gotErr <- err })

	assert.NoError(t, mw.WriteFrame(RandFrame(), nil))

	select {
	case err := <-gotErr:
		assert.Equal(t, err, writeErr)
	case <-time.After(5 * time.Second):
		t.Fatal("onError not called")
	}

	select {
	case <-mw.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done did not return")
	}
}

// Tests the critical deadlock path from the design doc:
// run() → Write fails → sets closed → onError → Stop() → noop → run() returns.
func TestMuxWriter_OnErrorCallingStopDoesNotDeadlock(t *testing.T) {
	writeErr := errors.New("broken pipe")
	fw := newFailWriter(1, writeErr)

	var mw *MuxWriter
	mw = NewMuxWriter(fw, func(err error) {
		// Simulate manager.terminate calling Stop.
		mw.Stop(errors.New("stopped"))
	})

	assert.NoError(t, mw.WriteFrame(RandFrame(), nil))

	select {
	case <-mw.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: Done did not return")
	}
}

// Tests the manager's two-phase shutdown: close transport to unblock a blocked
// Write, then Stop signals the goroutine to exit.
func TestMuxWriter_BlockedWriteUnblockedByClose(t *testing.T) {
	bw := newBlockingWriter()
	mw := NewMuxWriter(bw, func(error) {})

	assert.NoError(t, mw.WriteFrame(RandFrame(), nil))

	// Wait for run() to enter Write.
	select {
	case <-bw.wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("run() did not enter Write")
	}

	// Simulate terminate: Stop, then unblock the writer (like tr.Close()).
	mw.Stop(errors.New("stopped"))
	bw.err = errors.New("closed")
	close(bw.unblock)

	select {
	case <-mw.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: Done did not return")
	}
}

func TestMuxWriter_ConcurrentStop(t *testing.T) {
	mw := NewMuxWriter(io.Discard, func(error) {})

	// Write a frame so the goroutine has work.
	assert.NoError(t, mw.WriteFrame(RandFrame(), nil))

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			mw.Stop(errors.New("stopped"))
		}()
	}
	wg.Wait()

	select {
	case <-mw.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done did not return")
	}
}

// Stop has abort semantics: buffered data is discarded, not drained.
func TestMuxWriter_StopDiscardsBufferedData(t *testing.T) {
	bw := newBlockingWriter()
	mw := NewMuxWriter(bw, func(error) {})

	// Write several frames while the writer is blocked on the first Write.
	for range 10 {
		assert.NoError(t, mw.WriteFrame(RandFrame(), nil))
	}

	// Wait for run() to enter Write with the first batch.
	select {
	case <-bw.wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("run() did not enter Write")
	}

	// More frames accumulate in buf while Write is blocked.
	for range 10 {
		assert.NoError(t, mw.WriteFrame(RandFrame(), nil))
	}

	// Stop without letting the blocked Write complete.
	mw.Stop(errors.New("stopped"))
	bw.err = errors.New("closed")
	close(bw.unblock)

	select {
	case <-mw.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done did not return")
	}

	// Only the first batch was written; the rest were discarded by Stop.
	assert.Equal(t, len(bw.wrote), 0) // no more writes after the first
}

func TestMuxWriter_WriteFrameDuringActiveDrain(t *testing.T) {
	// gatedWriter lets us control when each Write completes.
	type gate struct{ ch chan struct{} }
	gates := make(chan gate, 10)

	gw := writerFunc(func(p []byte) (int, error) {
		g := gate{ch: make(chan struct{})}
		gates <- g
		<-g.ch
		return len(p), nil
	})

	mw := NewMuxWriter(gw, func(error) {})

	// Batch 1: write a frame, wait for run() to pick it up and block in Write.
	fr1 := Frame{Data: []byte("batch1"), ID: ID{Stream: 1, Message: 1}, Kind: KindMessage, Done: true}
	assert.NoError(t, mw.WriteFrame(fr1, nil))

	g1 := <-gates // run() is now blocked in Write for batch 1

	// Batch 2: write another frame while batch 1 is still draining.
	fr2 := Frame{Data: []byte("batch2"), ID: ID{Stream: 1, Message: 2}, Kind: KindMessage, Done: true}
	assert.NoError(t, mw.WriteFrame(fr2, nil))

	// Complete batch 1 write.
	close(g1.ch)

	// run() loops, picks up batch 2, enters Write again.
	g2 := <-gates
	close(g2.ch)

	// Both batches were written. Stop and verify.
	mw.Stop(errors.New("stopped"))
	<-mw.Done()
}

// writerFunc adapts a function to io.Writer.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// newTinyMuxWriter builds a MuxWriter with a 1-byte high-water mark so the
// next WriteFrame after one full frame parks on backpressure.
func newTinyMuxWriter(w io.Writer) *MuxWriter {
	return NewMuxWriterWithOptions(w, func(error) {}, WriterOptions{MaximumBufferSize: 1})
}

// blockUntilFull writes one frame that run() picks up and stalls on (leaving buf
// empty), then a second frame that fills buf past the limit. After it returns,
// run() is blocked in Write and the next WriteFrame is guaranteed to park. It
// requires a 1-byte high-water mark and a blockingWriter.
func blockUntilFull(t *testing.T, mw *MuxWriter, bw *blockingWriter) {
	t.Helper()
	assert.NoError(t, mw.WriteFrame(RandFrame(), nil))
	select {
	case <-bw.wrote: // run() is now stalled in Write; buf is empty
	case <-time.After(5 * time.Second):
		t.Fatal("run() did not enter Write")
	}
	assert.NoError(t, mw.WriteFrame(RandFrame(), nil)) // refills buf past the limit
}

// assertBlocked asserts that the pending WriteFrame on done has not returned.
func assertBlocked(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case <-done:
		t.Fatal("WriteFrame returned while it should have been blocked")
	case <-time.After(100 * time.Millisecond):
	}
}

// A full buffer parks WriteFrame until run() drains it, after which the parked
// call appends and returns.
func TestMuxWriter_WriteFrameBlocksUntilDrain(t *testing.T) {
	bw := newBlockingWriter()
	mw := newTinyMuxWriter(bw)
	blockUntilFull(t, mw, bw)

	done := make(chan error, 1)
	go func() { done <- mw.WriteFrame(RandFrame(), nil) }()
	assertBlocked(t, done)

	// Let the stalled Write complete; run() drains, swaps buf empty, and wakes
	// the parked writer, which then appends and returns.
	close(bw.unblock)
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("WriteFrame stayed blocked after drain")
	}

	mw.Stop(errors.New("stopped"))
	<-mw.Done()
}

// A parked WriteFrame returns errInterrupted when its cancel channel fires.
func TestMuxWriter_WriteFrameCanceledWhileBlocked(t *testing.T) {
	bw := newBlockingWriter()
	mw := newTinyMuxWriter(bw)
	blockUntilFull(t, mw, bw)

	cancel := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- mw.WriteFrame(RandFrame(), cancel) }()
	assertBlocked(t, done)

	close(cancel)
	select {
	case err := <-done:
		assert.Error(t, err)
		assert.Equal(t, err.Error(), errInterrupted.Error())
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not unblock WriteFrame")
	}

	// Cleanup: let run() finish and exit.
	bw.err = errors.New("closed")
	close(bw.unblock)
	mw.Stop(errors.New("stopped"))
	<-mw.Done()
}

// A control-bit frame is appended immediately even when the buffer is full, so
// an abortive cancel is never delayed by backpressure.
func TestMuxWriter_ControlFrameBypassesFullBuffer(t *testing.T) {
	bw := newBlockingWriter()
	mw := newTinyMuxWriter(bw)
	blockUntilFull(t, mw, bw)

	done := make(chan error, 1)
	go func() {
		fr := RandFrame()
		fr.Control = true
		done <- mw.WriteFrame(fr, nil)
	}()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("control frame did not bypass the full buffer")
	}

	// Cleanup: let run() finish the stalled Write and exit.
	bw.err = errors.New("closed")
	close(bw.unblock)
	mw.Stop(errors.New("stopped"))
	<-mw.Done()
}

// Stop wakes a parked WriteFrame even while run() is stuck in a slow Write, so
// shutdown does not depend on the buffer draining.
func TestMuxWriter_StopUnblocksBlockedWriteFrame(t *testing.T) {
	bw := newBlockingWriter()
	mw := newTinyMuxWriter(bw)
	blockUntilFull(t, mw, bw)

	done := make(chan error, 1)
	go func() { done <- mw.WriteFrame(RandFrame(), nil) }()
	assertBlocked(t, done)

	mw.Stop(errors.New("stopped"))
	select {
	case err := <-done:
		assert.Error(t, err)
		assert.Equal(t, err.Error(), "stopped")
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not unblock WriteFrame")
	}

	// run() is still stuck in Write; release it so the goroutine exits.
	bw.err = errors.New("closed")
	close(bw.unblock)
	<-mw.Done()
}
