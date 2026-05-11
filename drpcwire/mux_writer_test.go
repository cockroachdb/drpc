// Copyright (C) 2026 Cockroach Labs.
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

// syncBuf is a goroutine-safe bytes.Buffer.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.buf.Bytes()...)
}

func TestMuxWriter(t *testing.T) {
	var sb syncBuf
	mw := NewMuxWriter(&sb, func(error) {})

	var exp []byte
	for range 1000 {
		fr := RandFrame()
		exp = AppendFrame(exp, fr)
		assert.NoError(t, mw.WriteFrame(fr))
	}

	mw.Stop(errors.New("stopped"))
	<-mw.Done()

	assert.That(t, bytes.Equal(exp, sb.Bytes()))
}

func TestMuxWriter_WriteFrameAfterStop(t *testing.T) {
	mw := NewMuxWriter(io.Discard, func(error) {})
	mw.Stop(errors.New("stopped"))
	<-mw.Done()

	err := mw.WriteFrame(RandFrame())
	assert.Error(t, err)
	assert.Equal(t, err.Error(), "stopped")
}

func TestMuxWriter_ConcurrentWriteFrame(t *testing.T) {
	var sb syncBuf
	mw := NewMuxWriter(&sb, func(error) {})

	const numWriters = 10
	const framesPerWriter = 100

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
				assert.NoError(t, mw.WriteFrame(allFrames[i][j]))
			}
		}()
	}
	wg.Wait()

	mw.Stop(errors.New("stopped"))
	<-mw.Done()

	got := sb.Bytes()
	assert.Equal(t, len(got), expSize)

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

func TestMuxWriter_WriteErrorReturnsError(t *testing.T) {
	writeErr := errors.New("disk full")
	fw := newFailWriter(1, writeErr)

	gotErr := make(chan error, 1)
	mw := NewMuxWriter(fw, func(err error) { gotErr <- err })

	// The caller-leader runs the failing Write itself and surfaces the error.
	err := mw.WriteFrame(RandFrame())
	assert.Equal(t, err, writeErr)

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

// Tests the critical deadlock path: WriteFrame fails → sets closed → onError
// → Stop() → noop → run() returns.
func TestMuxWriter_OnErrorCallingStopDoesNotDeadlock(t *testing.T) {
	writeErr := errors.New("broken pipe")
	fw := newFailWriter(1, writeErr)

	var mw *MuxWriter
	mw = NewMuxWriter(fw, func(err error) {
		// Simulate manager.terminate calling Stop.
		mw.Stop(errors.New("stopped"))
	})

	err := mw.WriteFrame(RandFrame())
	assert.Equal(t, err, writeErr)

	select {
	case <-mw.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: Done did not return")
	}
}

// Tests the manager's two-phase shutdown: close transport to unblock a blocked
// Write, then Stop signals the writers to exit.
func TestMuxWriter_BlockedWriteUnblockedByClose(t *testing.T) {
	bw := newBlockingWriter()
	mw := NewMuxWriter(bw, func(error) {})

	// WriteFrame becomes the caller-leader and blocks in transport.Write.
	errCh := make(chan error, 1)
	go func() { errCh <- mw.WriteFrame(RandFrame()) }()

	// Wait for the caller-leader to enter Write.
	select {
	case <-bw.wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("WriteFrame did not enter Write")
	}

	// Simulate terminate: Stop, then unblock the writer.
	mw.Stop(errors.New("stopped"))
	bw.err = errors.New("closed")
	close(bw.unblock)

	select {
	case <-mw.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: Done did not return")
	}

	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("WriteFrame did not return")
	}
}

func TestMuxWriter_ConcurrentStop(t *testing.T) {
	mw := NewMuxWriter(io.Discard, func(error) {})

	// Write a frame so there's been activity before Stop.
	assert.NoError(t, mw.WriteFrame(RandFrame()))

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

	// First WriteFrame becomes leader and blocks in Write.
	errCh := make(chan error, 1)
	go func() { errCh <- mw.WriteFrame(RandFrame()) }()

	// Wait for the leader to enter Write.
	select {
	case <-bw.wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("WriteFrame did not enter Write")
	}

	// All subsequent writes go through the slow path and queue immediately.
	for range 19 {
		assert.NoError(t, mw.WriteFrame(RandFrame()))
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

	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("WriteFrame did not return")
	}

	// Only the first batch was written; the queued frames were discarded.
	assert.Equal(t, len(bw.wrote), 0)
}

func TestMuxWriter_WriteFrameDuringActiveDrain(t *testing.T) {
	type gate struct{ ch chan struct{} }
	gates := make(chan gate, 10)

	gw := writerFunc(func(p []byte) (int, error) {
		g := gate{ch: make(chan struct{})}
		gates <- g
		<-g.ch
		return len(p), nil
	})

	mw := NewMuxWriter(gw, func(error) {})

	// First WriteFrame becomes leader and blocks in Write for batch 1.
	fr1 := Frame{Data: []byte("batch1"), ID: ID{Stream: 1, Message: 1}, Kind: KindMessage, Done: true}
	leader1Done := make(chan error, 1)
	go func() { leader1Done <- mw.WriteFrame(fr1) }()

	g1 := <-gates // leader is blocked in Write for batch 1

	// While leader is blocked, a follower queues batch 2 and Flushes (no-op
	// because writing=true).
	fr2 := Frame{Data: []byte("batch2"), ID: ID{Stream: 1, Message: 2}, Kind: KindMessage, Done: true}
	assert.NoError(t, mw.WriteFrame(fr2))

	// Complete batch 1; leader's drain loop sees buf still has batch 2 and
	// calls Write inline before returning.
	close(g1.ch)
	g2 := <-gates
	close(g2.ch)

	assert.NoError(t, <-leader1Done)

	mw.Stop(errors.New("stopped"))
	<-mw.Done()
}

// writerFunc adapts a function to io.Writer.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
