// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/zeebo/assert"
	"github.com/zeebo/errs"

	"storj.io/drpc"
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

// Nonconforming grant frames are dropped by the intercept without crediting.
func TestStream_MalformedGrantIgnored(t *testing.T) {
	st := newGateStream(t)
	st.sendw = newSendWindow(0)

	missing := drpcwire.WindowUpdateFrame(st.ID(), 1)
	missing.Data = nil // no delta payload
	assert.NoError(t, st.HandleFrame(missing))
	assert.Equal(t, st.sendw.available(), int64(0))

	zero := drpcwire.WindowUpdateFrame(st.ID(), 1)
	zero.Data = drpcwire.AppendVarint(nil, 0) // zero delta
	assert.NoError(t, st.HandleFrame(zero))
	assert.Equal(t, st.sendw.available(), int64(0))
}

// A grant arriving after termination is dropped.
func TestStream_GrantAfterTerminationDropped(t *testing.T) {
	st := newGateStream(t)
	st.sendw = newSendWindow(0)

	st.Cancel(errs.New("boom"))
	assert.NoError(t, st.HandleFrame(drpcwire.WindowUpdateFrame(st.ID(), 100)))
	assert.Equal(t, st.sendw.available(), int64(0))
}

// An unfinished message superseded by a higher message id must release its
// buffered bytes, or the high-water gate sticks shut and the withheld grant
// is never emitted (sender deadlock once its credit is exhausted).
func TestStream_DiscardedPartialMessageReleasesCredit(t *testing.T) {
	mw, frames := captureWriter(t)
	st := NewWithOptions(context.Background(), 1, mw, NewBufferPool(), Options{SplitSize: 64 << 10})
	st.recvw = newRecvWindow(100, 50)
	sid := st.ID()

	// Partial message 1 reaches the high-water mark: grants withheld.
	assert.NoError(t, st.HandleFrame(msgFrame(sid, 1, make([]byte, 120), false)))
	assertNoFrame(t, frames)

	// Message 2 supersedes it; the discarded 120 bytes leave the buffer after
	// the replacement is counted, the gate reopens, and all accrued credit
	// (120 discarded + 10 replacement) flushes.
	assert.NoError(t, st.HandleFrame(msgFrame(sid, 2, make([]byte, 10), true)))

	_, delta, ok := drpcwire.ParseWindowUpdate(waitFrame(t, frames))
	assert.That(t, ok)
	assert.Equal(t, delta, uint64(130))

	// The completed message is intact and its own credit accounting holds.
	got, err := st.RawRecv()
	assert.NoError(t, err)
	assert.Equal(t, len(got), 10)
	assert.Equal(t, st.recvw.bufferedBytes(), int64(0))
}

// The discard release must not decide on the transient dip before the
// replacement frame is counted: a large replacement that lands above
// high-water withholds everything, including the discarded bytes' credit.
func TestStream_DiscardReleaseSeesReplacementFrame(t *testing.T) {
	mw, frames := captureWriter(t)
	st := NewWithOptions(context.Background(), 1, mw, NewBufferPool(), Options{SplitSize: 64 << 10})
	st.recvw = newRecvWindow(100, 50)
	sid := st.ID()

	// Partial message 1 pins buffered at the high-water mark.
	assert.NoError(t, st.HandleFrame(msgFrame(sid, 1, make([]byte, 120), false)))
	assertNoFrame(t, frames)

	// Message 2 (1000 bytes) supersedes it. Final buffered = 1000 >= 100, so
	// no grant may be emitted -- releasing the 120 on the dip would be wrong.
	assert.NoError(t, st.HandleFrame(msgFrame(sid, 2, make([]byte, 1000), true)))
	assertNoFrame(t, frames)

	// Consuming the completed message reopens the gate and flushes everything.
	_, err := st.RawRecv()
	assert.NoError(t, err)
	_, delta, ok := drpcwire.ParseWindowUpdate(waitFrame(t, frames))
	assert.That(t, ok)
	assert.Equal(t, delta, uint64(1120))
}

// A terminal frame that supersedes a partial message releases the discarded
// bytes inline, before termination is applied, so buffered stays balanced. That
// emits one window update for credit the peer will not spend (it sent the
// terminal frame) -- benign, and not worth a deferred release to suppress.
func TestStream_TerminalSupersedeReleasesDiscarded(t *testing.T) {
	mw, frames := captureWriter(t)
	st := NewWithOptions(context.Background(), 1, mw, NewBufferPool(), Options{SplitSize: 64 << 10})
	st.recvw = newRecvWindow(100, 50)
	sid := st.ID()

	// Partial message 1: grants withheld above high-water.
	assert.NoError(t, st.HandleFrame(msgFrame(sid, 1, make([]byte, 120), false)))
	assertNoFrame(t, frames)

	// A remote cancel with a higher message id discards the partial and
	// terminates the stream. The 120 discarded bytes are released (one benign
	// grant) and buffered returns to zero.
	assert.NoError(t, st.HandleFrame(drpcwire.Frame{
		ID: drpcwire.ID{Stream: sid, Message: 2}, Kind: drpcwire.KindCancel, Done: true,
	}))
	assert.That(t, st.IsTerminated())

	_, delta, ok := drpcwire.ParseWindowUpdate(waitFrame(t, frames))
	assert.That(t, ok)
	assert.Equal(t, delta, uint64(120))
	assert.Equal(t, st.recvw.bufferedBytes(), int64(0))
}

// Draining queued messages after termination must not emit grants: credit
// returned behind the terminal frame would invite more doomed data.
func TestStream_NoGrantsAfterTermination(t *testing.T) {
	mw, frames := captureWriter(t)
	st := NewWithOptions(context.Background(), 1, mw, NewBufferPool(), Options{SplitSize: 64 << 10})
	st.recvw = newRecvWindow(100, 50)

	// 120 bytes buffered >= high-water 100: grant withheld while live.
	assert.NoError(t, st.HandleFrame(msgFrame(st.ID(), 1, make([]byte, 120), true)))
	assertNoFrame(t, frames)

	st.Cancel(errs.New("boom"))

	// The queued message still drains (ring close keeps buffered data
	// readable), which on a live stream would flush the accrued 120 bytes of
	// credit; after termination it must stay unemitted.
	got, err := st.RawRecv()
	assert.NoError(t, err)
	assert.Equal(t, len(got), 120)
	assertNoFrame(t, frames)
}

// rawEnc round-trips raw byte slices for MsgRecv tests.
type rawEnc struct{}

func (rawEnc) Marshal(msg drpc.Message) ([]byte, error) { return *(msg.(*[]byte)), nil }
func (rawEnc) Unmarshal(buf []byte, msg drpc.Message) error {
	*(msg.(*[]byte)) = append([]byte(nil), buf...)
	return nil
}

// The MsgRecv consume path also returns credit, like RawRecv.
func TestStream_MsgRecvEmitsGrant(t *testing.T) {
	mw, frames := captureWriter(t)
	st := NewWithOptions(context.Background(), 1, mw, NewBufferPool(), Options{SplitSize: 64 << 10})
	st.recvw = newRecvWindow(100, 50)

	// 120 bytes buffered >= high-water 100 -> withheld on dispatch.
	assert.NoError(t, st.HandleFrame(msgFrame(st.ID(), 1, make([]byte, 120), true)))
	assertNoFrame(t, frames)

	var msg []byte
	assert.NoError(t, st.MsgRecv(&msg, rawEnc{}))
	assert.Equal(t, len(msg), 120)

	_, delta, ok := drpcwire.ParseWindowUpdate(waitFrame(t, frames))
	assert.That(t, ok)
	assert.Equal(t, delta, uint64(120))
}

// A message that fails to decompress still releases its bytes to the receive
// window. The bytes have left the queue, so buffered must not stay inflated --
// otherwise the high-water gate could wedge credit shut after a bad frame.
func TestStream_DecompressErrorReleasesCredit(t *testing.T) {
	mw := testMuxWriter(t)
	opts := Options{SplitSize: 64 << 10, Compression: drpc.CompressionSnappy}
	st := NewWithOptions(context.Background(), 1, mw, NewBufferPool(), opts)
	st.recvw = newRecvWindow(1<<20, 1<<20) // high threshold: no grant to mask the release

	// A KindMessage payload that is not valid snappy.
	bad := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	assert.NoError(t, st.HandleFrame(msgFrame(st.ID(), 1, bad, true)))
	assert.Equal(t, st.recvw.bufferedBytes(), int64(len(bad))) // dispatched, not yet consumed

	// RawRecv fails to decompress, but the bytes must still be released.
	_, err := st.RawRecv()
	assert.Error(t, err)
	assert.Equal(t, st.recvw.bufferedBytes(), int64(0))
}
