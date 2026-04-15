// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcwire

import (
	"io"
	"sync"
)

type MuxWriter struct {
	w        io.Writer
	buf      []byte
	mu       sync.Mutex
	cond     *sync.Cond
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
		onError: onError,
		done:    make(chan struct{}),
	}
	mw.cond = sync.NewCond(&mw.mu)
	go mw.run()
	return mw
}

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

		mw.buf, spare = spare, mw.buf
		mw.mu.Unlock()
		if _, err := mw.w.Write(spare); err != nil {
			mw.mu.Lock()
			if mw.closed {
				mw.mu.Unlock()
				return
			}
			mw.closed = true
			mw.closeErr = err
			mw.mu.Unlock()
			if mw.onError != nil {
				mw.onError(err)
			}
			return
		}

		spare = spare[:0]
	}
}

func (mw *MuxWriter) WriteFrame(fr Frame) (err error) {
	mw.mu.Lock()
	defer mw.mu.Unlock()
	if mw.closed {
		return mw.closeErr
	}
	mw.buf = AppendFrame(mw.buf, fr)
	mw.cond.Signal()
	return nil
}

func (mw *MuxWriter) Stop(err error) {
	mw.mu.Lock()
	if !mw.closed {
		mw.closed = true
		mw.closeErr = err
		mw.cond.Broadcast()
	}
	mw.mu.Unlock()
}

func (mw *MuxWriter) Done() <-chan struct{} {
	return mw.done
}
