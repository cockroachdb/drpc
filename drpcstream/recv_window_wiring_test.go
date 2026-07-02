// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/zeebo/assert"

	"storj.io/drpc/drpcwire"
)

// captureWriter returns a MuxWriter whose output frames are decoded and
// delivered on the returned channel, so tests can observe emitted grants.
func captureWriter(t *testing.T) (*drpcwire.MuxWriter, <-chan drpcwire.Frame) {
	t.Helper()
	pr, pw := io.Pipe()
	mw := drpcwire.NewMuxWriter(pw, func(error) {})
	frames := make(chan drpcwire.Frame, 64)
	rd := drpcwire.NewReader(pr)
	go func() {
		for {
			fr, err := rd.ReadFrame()
			if err != nil {
				return
			}
			frames <- fr
		}
	}()
	t.Cleanup(func() {
		mw.Stop(nil)
		<-mw.Done()
		_ = pw.Close()
		_ = pr.Close()
	})
	return mw, frames
}

func waitFrame(t *testing.T, frames <-chan drpcwire.Frame) drpcwire.Frame {
	t.Helper()
	select {
	case fr := <-frames:
		return fr
	case <-time.After(time.Second):
		t.Fatal("expected a frame to be written, got none")
		return drpcwire.Frame{}
	}
}

func assertNoFrame(t *testing.T, frames <-chan drpcwire.Frame) {
	t.Helper()
	select {
	case fr := <-frames:
		t.Fatalf("unexpected frame written: %v", fr)
	case <-time.After(blockShort):
	}
}

func msgFrame(sid, mid uint64, data []byte, done bool) drpcwire.Frame {
	return drpcwire.Frame{
		ID:   drpcwire.ID{Stream: sid, Message: mid},
		Kind: drpcwire.KindMessage,
		Data: data,
		Done: done,
	}
}

// An incoming KindWindowUpdate is intercepted and applied to the send window.
func TestStream_IncomingGrantAppliesToSendWindow(t *testing.T) {
	st := newGateStream(t)
	st.sendw = newSendWindow(0)

	assert.NoError(t, st.HandleFrame(drpcwire.WindowUpdateFrame(st.ID(), 500)))
	assert.Equal(t, st.sendw.available(), int64(500))
}

// A grant interleaved with an in-progress message is intercepted before the
// assembler, so reassembly is undisturbed.
func TestStream_GrantDoesNotDisturbReassembly(t *testing.T) {
	st := newGateStream(t)
	sid := st.ID()

	assert.NoError(t, st.HandleFrame(msgFrame(sid, 1, []byte("foo"), false)))
	assert.NoError(t, st.HandleFrame(drpcwire.WindowUpdateFrame(sid, 100))) // interleaved
	assert.NoError(t, st.HandleFrame(msgFrame(sid, 1, []byte("bar"), true)))

	got, err := st.RawRecv()
	assert.NoError(t, err)
	assert.Equal(t, string(got), "foobar")
}

// Dispatching a data frame returns credit once the threshold is met.
func TestStream_DispatchEmitsGrant(t *testing.T) {
	mw, frames := captureWriter(t)
	st := NewWithOptions(context.Background(), 1, mw, NewBufferPool(), Options{SplitSize: 64 << 10})
	st.recvw = newRecvWindow(1<<20, 100)

	assert.NoError(t, st.HandleFrame(msgFrame(st.ID(), 1, make([]byte, 150), true)))

	fr := waitFrame(t, frames)
	sid, delta, ok := drpcwire.ParseWindowUpdate(fr)
	assert.That(t, ok)
	assert.Equal(t, sid, st.ID())
	assert.Equal(t, delta, uint64(150))
}

// Above the high-water mark, dispatch withholds; consuming resumes and flushes
// the accrued credit.
func TestStream_ConsumeEmitsGrantOnResume(t *testing.T) {
	mw, frames := captureWriter(t)
	st := NewWithOptions(context.Background(), 1, mw, NewBufferPool(), Options{SplitSize: 64 << 10})
	st.recvw = newRecvWindow(100, 50)

	// 120 bytes buffered >= high-water 100 -> withheld.
	assert.NoError(t, st.HandleFrame(msgFrame(st.ID(), 1, make([]byte, 120), true)))
	assertNoFrame(t, frames)

	// Consume -> buffered drops below high-water -> flush accrued 120.
	_, err := st.RawRecv()
	assert.NoError(t, err)

	fr := waitFrame(t, frames)
	_, delta, ok := drpcwire.ParseWindowUpdate(fr)
	assert.That(t, ok)
	assert.Equal(t, delta, uint64(120))
}

// With no receive window installed, no grants are emitted and an incoming grant
// with no send window is harmlessly ignored.
func TestStream_NoWindowsNoGrants(t *testing.T) {
	mw, frames := captureWriter(t)
	st := NewWithOptions(context.Background(), 1, mw, NewBufferPool(), Options{SplitSize: 64 << 10})

	assert.NoError(t, st.HandleFrame(msgFrame(st.ID(), 1, make([]byte, 150), true)))
	assertNoFrame(t, frames)

	assert.NoError(t, st.HandleFrame(drpcwire.WindowUpdateFrame(st.ID(), 100)))
}

// The receiver defends against a peer that does not honor the max-message
// bound: an incoming message that grows past the implicit maximum (high_water +
// window) can never complete, so the stream is failed with a data-overflow
// error and the peer is notified with an abortive cancel. The connection stays
// up (HandleFrame returns nil).
func TestStream_ReceiverRejectsOversizedMessage(t *testing.T) {
	mw, frames := captureWriter(t)
	st := NewWithOptions(context.Background(), 1, mw, NewBufferPool(), Options{
		SplitSize:   64 << 10,
		FlowControl: FlowControl{Enabled: true, StreamWindow: 256, HighWater: 1024, GrantThreshold: 128},
	})
	sid := st.ID()
	// maxMsg = HighWater + StreamWindow = 1280.
	assert.NoError(t, st.HandleFrame(msgFrame(sid, 1, make([]byte, 1024), false)))
	// This frame pushes the in-progress message to 1324 > 1280 without a Done,
	// so it can never complete: the receiver must fail the stream.
	assert.NoError(t, st.HandleFrame(msgFrame(sid, 1, make([]byte, 300), false)))

	// The peer is notified with a cancel.
	fr := waitFrame(t, frames)
	assert.Equal(t, fr.Kind, drpcwire.KindCancel)

	// The local stream is failed with the data-overflow error.
	_, err := st.RawRecv()
	assert.Error(t, err)
	assert.That(t, strings.Contains(err.Error(), "exceeds"))
}
