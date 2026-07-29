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
	"storj.io/drpc/drpcmetrics"
	"storj.io/drpc/drpcwire"
)

// Consuming returns credit once the accrued amount reaches the threshold, and
// withholds (0) below it; the accrued credit resets after a grant.
func TestRecvWindowGrantsAtThreshold(t *testing.T) {
	w := newRecvWindow(100)
	assert.Equal(t, w.consumed(60), int64(0))   // pending 60 < 100
	assert.Equal(t, w.consumed(60), int64(120)) // pending 120 >= 100 -> grant, reset
	assert.Equal(t, w.consumed(50), int64(0))   // pending 50 < 100 again
}

// Many small consumes coalesce into few grants, each carrying the accrued
// amount, and no credit is lost across grants.
func TestRecvWindowCoalescesGrants(t *testing.T) {
	w := newRecvWindow(100)
	grants, granted := 0, int64(0)
	for i := 0; i < 10; i++ {
		if g := w.consumed(30); g > 0 {
			grants++
			granted += g
		}
	}
	// 10 consumes of 30 (300 bytes) with threshold 100 coalesce into 2 grants
	// of 120 each; the remaining 60 stays accrued.
	assert.Equal(t, grants, 2)
	assert.Equal(t, granted, int64(240))
}

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

func recvStream(t *testing.T, threshold int64) (*Stream, <-chan drpcwire.Frame) {
	mw, frames := captureWriter(t)
	st := NewWithOptions(context.Background(), 1, mw, NewBufferPool(), drpcmetrics.ConnectionMetrics{}, Options{SplitSize: 64 << 10})
	st.recvw = newRecvWindow(threshold)
	return st, frames
}

// Credit is returned on consume, not on receipt: receiving a message emits
// nothing, and reading it emits the grant once past the threshold.
func TestStream_GrantOnConsumeNotReceipt(t *testing.T) {
	st, frames := recvStream(t, 100)

	assert.NoError(t, st.HandleFrame(msgFrame(st.ID(), 1, make([]byte, 150), true)))
	assertNoFrame(t, frames) // receipt returns no credit

	_, err := st.RawRecv()
	assert.NoError(t, err)

	sid, delta, ok := drpcwire.ParseWindowUpdate(waitFrame(t, frames))
	assert.That(t, ok)
	assert.Equal(t, sid, st.ID())
	assert.Equal(t, delta, uint64(150))
}

// Consumes below the threshold emit nothing; the consume that crosses it
// flushes all accrued credit as one coalesced window update.
func TestStream_ConsumeCoalescesGrants(t *testing.T) {
	st, frames := recvStream(t, 100)
	sid := st.ID()

	assert.NoError(t, st.HandleFrame(msgFrame(sid, 1, make([]byte, 60), true)))
	assert.NoError(t, st.HandleFrame(msgFrame(sid, 2, make([]byte, 90), true)))

	_, err := st.RawRecv() // 60 < 100: withheld
	assert.NoError(t, err)
	assertNoFrame(t, frames)

	_, err = st.RawRecv() // 60 + 90 = 150 >= 100: flush
	assert.NoError(t, err)
	_, delta, ok := drpcwire.ParseWindowUpdate(waitFrame(t, frames))
	assert.That(t, ok)
	assert.Equal(t, delta, uint64(150))
}

// The MsgRecv consume path returns credit like RawRecv.
func TestStream_MsgRecvEmitsGrant(t *testing.T) {
	st, frames := recvStream(t, 50)

	assert.NoError(t, st.HandleFrame(msgFrame(st.ID(), 1, make([]byte, 120), true)))
	assertNoFrame(t, frames)

	var msg []byte
	assert.NoError(t, st.MsgRecv(&msg, rawEnc{}))
	assert.Equal(t, len(msg), 120)

	_, delta, ok := drpcwire.ParseWindowUpdate(waitFrame(t, frames))
	assert.That(t, ok)
	assert.Equal(t, delta, uint64(120))
}

// A message that fails to decode still releases its bytes: the grant is emitted
// before decoding, so a decode error cannot lose the credit.
func TestStream_DecodeErrorStillGrants(t *testing.T) {
	mw, frames := captureWriter(t)
	opts := Options{SplitSize: 64 << 10, Compression: drpc.CompressionSnappy}
	st := NewWithOptions(context.Background(), 1, mw, NewBufferPool(), drpcmetrics.ConnectionMetrics{}, opts)
	st.recvw = newRecvWindow(1) // grant on every consume

	// A KindMessage payload that is not valid snappy.
	bad := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	assert.NoError(t, st.HandleFrame(msgFrame(st.ID(), 1, bad, true)))

	_, err := st.RawRecv()
	assert.Error(t, err)

	_, delta, ok := drpcwire.ParseWindowUpdate(waitFrame(t, frames))
	assert.That(t, ok)
	assert.Equal(t, delta, uint64(len(bad)))
}

// Draining queued messages after termination must not emit grants: credit
// returned behind the terminal frame would invite more doomed data.
func TestStream_NoGrantsAfterTermination(t *testing.T) {
	st, frames := recvStream(t, 50)

	assert.NoError(t, st.HandleFrame(msgFrame(st.ID(), 1, make([]byte, 120), true)))
	st.Cancel(errs.New("boom"))

	// The queued message still drains, which on a live stream would flush 120
	// bytes of credit; after termination it must stay unemitted.
	got, err := st.RawRecv()
	assert.NoError(t, err)
	assert.Equal(t, len(got), 120)
	assertNoFrame(t, frames)
}

// An incoming KindWindowUpdate is intercepted and applied to the send window.
func TestStream_IncomingGrantAppliesToSendWindow(t *testing.T) {
	st := newGateStream(t)
	st.sendw = newSendWindow(0, st.sigs.send.Signal(), st.sigs.send.Err)

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

// rawEnc round-trips raw byte slices for MsgRecv tests.
type rawEnc struct{}

func (rawEnc) Marshal(msg drpc.Message) ([]byte, error) { return *(msg.(*[]byte)), nil }
func (rawEnc) Unmarshal(buf []byte, msg drpc.Message) error {
	*(msg.(*[]byte)) = append([]byte(nil), buf...)
	return nil
}
