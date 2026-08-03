// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import (
	"context"
	"errors"
	"io"
	"math"
	"testing"
	"time"

	"github.com/zeebo/assert"
	"github.com/zeebo/errs"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"storj.io/drpc"
	"storj.io/drpc/drpcmetrics"
	"storj.io/drpc/drpcwire"
	"storj.io/drpc/internal/drpcopts"
)

// newGateStream builds a stream writing to io.Discard with an explicit
// SplitSize so small payloads are a single frame.
func newGateStream(t *testing.T) *Stream {
	mw := testMuxWriter(t)
	return NewWithOptions(
		context.Background(), 1, mw, NewBufferPool(),
		drpcmetrics.ConnectionMetrics{}, Options{SplitSize: 64 << 10},
	)
}

// By default no send window is installed, so data writes are ungated
// (unlimited) and behavior is unchanged.
func TestStream_SendWindowDefaultUngated(t *testing.T) {
	st := newGateStream(t)
	assert.That(t, st.sendw == nil)
	assert.NoError(t, st.RawWrite(drpcwire.KindMessage, []byte("hello")))
}

// With a finite send window, a data write blocks until enough credit is
// granted.
func TestStream_SendWindowGatesDataWrite(t *testing.T) {
	st := newGateStream(t)
	st.sendw = newSendWindow(4, st.sigs.send.Signal(), st.sigs.send.Err) // 4 bytes of credit

	done := make(chan error, 1)
	go func() { done <- st.RawWrite(drpcwire.KindMessage, []byte("hello")) }() // 5 bytes > 4

	select {
	case <-done:
		t.Fatal("data write returned before sufficient credit")
	case <-time.After(blockShort):
	}

	st.sendw.grant(1) // 4 + 1 = 5 >= 5

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("data write did not complete after grant")
	}
}

// Control kinds (here, invoke) are not flow-controlled: they proceed even with
// zero send credit.
func TestStream_SendWindowControlKindsBypassGate(t *testing.T) {
	st := newGateStream(t)
	st.sendw = newSendWindow(0, st.sigs.send.Signal(), st.sigs.send.Err) // no credit at all

	assert.NoError(t, st.WriteInvoke("service.Method", nil))
}

// SendCancel preempts a send parked on credit: it terminates (closing the
// window) before taking the write lock, so the parked write wakes, releases the
// lock, and the cancel frame goes out.
func TestStream_SendWindowSendCancelPreemptsParkedWrite(t *testing.T) {
	st := newGateStream(t)
	st.sendw = newSendWindow(0, st.sigs.send.Signal(), st.sigs.send.Err) // send will park immediately

	done := make(chan error, 1)
	go func() { done <- st.RawWrite(drpcwire.KindMessage, []byte("data")) }()

	select {
	case <-done:
		t.Fatal("data write returned before cancel")
	case <-time.After(blockShort):
	}

	assert.NoError(t, st.SendCancel(context.Canceled))

	select {
	case err := <-done:
		// Same error as a send parked in WriteFrame or a later send would see.
		assert.That(t, errors.Is(err, io.EOF))
	case <-time.After(time.Second):
		t.Fatal("parked data write was not preempted by SendCancel")
	}

	// A subsequent send observes the same error as the parked one.
	assert.That(t, errors.Is(st.RawWrite(drpcwire.KindMessage, []byte("more")), io.EOF))
}

// Terminating the stream wakes a send parked on credit.
func TestStream_SendWindowTerminateWakesParkedWrite(t *testing.T) {
	st := newGateStream(t)
	st.sendw = newSendWindow(0, st.sigs.send.Signal(), st.sigs.send.Err) // send will park immediately

	done := make(chan error, 1)
	go func() { done <- st.RawWrite(drpcwire.KindMessage, []byte("data")) }()

	select {
	case <-done:
		t.Fatal("data write returned before termination")
	case <-time.After(blockShort):
	}

	st.Cancel(errs.New("boom"))

	select {
	case err := <-done:
		// Cancel pre-sets sigs.send to io.EOF; the window closes with it.
		assert.That(t, errors.Is(err, io.EOF))
	case <-time.After(time.Second):
		t.Fatal("parked data write did not wake on termination")
	}
}

// fcOptions returns stream Options with flow control enabled via the internal
// option, the only way to enable it until it is promoted to a public option.
// MaxMessageSize is left unset, so the install site defaults it.
func fcOptions(window, threshold int64) Options {
	opts := Options{SplitSize: 64 << 10}
	drpcopts.SetStreamFlowControl(&opts.Internal, drpcopts.FlowControl{
		Enabled:        true,
		StreamWindow:   window,
		GrantThreshold: threshold,
	})
	return opts
}

// smallFCStream returns a stream with a tiny window so byte-level gating and
// overdraft are easy to exercise. Frames are 2 bytes and the grant threshold 2.
func smallFCStream(t *testing.T, window, maxMsg int64) *Stream {
	opts := Options{SplitSize: 2}
	drpcopts.SetStreamFlowControl(&opts.Internal, drpcopts.FlowControl{
		Enabled: true, StreamWindow: window, GrantThreshold: 2, MaxMessageSize: maxMsg,
	})
	return NewWithOptions(context.Background(), 1, testMuxWriter(t), NewBufferPool(), drpcmetrics.ConnectionMetrics{}, opts)
}

// Enabling flow control installs the send and receive windows, seeding the
// send window with the configured stream window as initial credit.
func TestStream_FlowControlEnabledInstallsWindows(t *testing.T) {
	mw := testMuxWriter(t)
	st := NewWithOptions(context.Background(), 1, mw, NewBufferPool(), drpcmetrics.ConnectionMetrics{}, fcOptions(256<<10, 64<<10))
	assert.That(t, st.sendw != nil)
	assert.That(t, st.recvw != nil)
	assert.Equal(t, st.sendw.available(), int64(256<<10))
}

// Without flow control enabled, no windows are installed and behavior is
// unchanged (ungated).
func TestStream_FlowControlDisabledNoWindows(t *testing.T) {
	mw := testMuxWriter(t)
	st := NewWithOptions(context.Background(), 1, mw, NewBufferPool(), drpcmetrics.ConnectionMetrics{}, Options{SplitSize: 64 << 10})
	assert.That(t, st.sendw == nil)
	assert.That(t, st.recvw == nil)
}

// An invalid flow-control configuration resorts to defaults instead of failing,
// so a misconfiguration degrades to a working stream rather than crashing a
// process that embeds drpc.
func TestStream_FlowControlDefaultsOnInvalid(t *testing.T) {
	const defaultWindow = int64(2 << 20) // drpcopts.defaultStreamWindow

	defaulted := func(name string, opts Options) {
		t.Run(name, func(t *testing.T) {
			st := NewWithOptions(context.Background(), 1, testMuxWriter(t), NewBufferPool(), drpcmetrics.ConnectionMetrics{}, opts)
			assert.Equal(t, st.sendw.available(), defaultWindow)
		})
	}

	defaulted("zero window", fcOptions(0, 128<<10))
	defaulted("zero threshold", fcOptions(256<<10, 0))
	defaulted("negative window", fcOptions(-1, 128<<10))
	// Threshold + frame must fit in the window, or a quiescent receiver can
	// strand the sender below the next frame's cost.
	defaulted("threshold+frame exceeds window", fcOptions(128<<10, 128<<10))
	// A near-max threshold must not sneak past the sizing check.
	defaulted("near-max threshold", fcOptions(256<<10, math.MaxInt64))

	// Flow control needs a bounded frame, so a non-positive SplitSize defaults to
	// DefaultFrameSize -- applied to opts so SplitData frames the same size on the
	// wire. A negative SplitSize, which SplitData alone would treat as unbounded,
	// is defaulted too (not left unbounded) once flow control is enabled. The rest
	// of each config is valid, so the window is kept.
	def := fcOptions(128<<10, 64<<10)
	def.SplitSize = 0 // unset
	st := NewWithOptions(context.Background(), 1, testMuxWriter(t), NewBufferPool(), drpcmetrics.ConnectionMetrics{}, def)
	assert.Equal(t, st.sendw.available(), int64(128<<10))         // valid: kept
	assert.Equal(t, st.opts.SplitSize, drpcwire.DefaultFrameSize) // defaulted

	unbounded := fcOptions(256<<10, 64<<10)
	unbounded.SplitSize = -1 // negative: SplitData alone would leave frames unbounded
	st = NewWithOptions(context.Background(), 1, testMuxWriter(t), NewBufferPool(), drpcmetrics.ConnectionMetrics{}, unbounded)
	assert.Equal(t, st.sendw.available(), int64(256<<10))         // valid: kept
	assert.Equal(t, st.opts.SplitSize, drpcwire.DefaultFrameSize) // defaulted, not left unbounded

	// A user SplitSize larger than its window is invalid, so window and threshold
	// fall back to defaults -- but the 512 KiB frame still fits the 2 MiB default
	// window, so it is preserved.
	bigFits := fcOptions(128<<10, 64<<10)
	bigFits.SplitSize = 512 << 10
	st = NewWithOptions(context.Background(), 1, testMuxWriter(t), NewBufferPool(), drpcmetrics.ConnectionMetrics{}, bigFits)
	assert.Equal(t, st.sendw.available(), defaultWindow) // window defaulted
	assert.Equal(t, st.opts.SplitSize, 512<<10)          // frame fits default, kept

	// A user SplitSize too large even for the default window is defaulted along
	// with window and threshold, so the three stay mutually consistent.
	bigNoFit := fcOptions(0, 64<<10) // invalid window forces defaults
	bigNoFit.SplitSize = 4 << 20     // 4 MiB > 2 MiB default window
	st = NewWithOptions(context.Background(), 1, testMuxWriter(t), NewBufferPool(), drpcmetrics.ConnectionMetrics{}, bigNoFit)
	assert.Equal(t, st.sendw.available(), defaultWindow)          // window defaulted
	assert.Equal(t, st.opts.SplitSize, drpcwire.DefaultFrameSize) // frame defaulted too
}

// A MaxMessageSize stricter than the window is valid (and safer -- the sender
// never overdrafts past it), so it is kept, not silently expanded to the
// default.
func TestStream_FlowControlStricterBoundKept(t *testing.T) {
	opts := Options{SplitSize: 64 << 10}
	drpcopts.SetStreamFlowControl(&opts.Internal, drpcopts.FlowControl{
		Enabled: true, StreamWindow: 256 << 10, GrantThreshold: 64 << 10, MaxMessageSize: 128 << 10,
	})
	st := NewWithOptions(context.Background(), 1, testMuxWriter(t), NewBufferPool(), drpcmetrics.ConnectionMetrics{}, opts)
	assert.Equal(t, st.sendw.available(), int64(256<<10)) // window kept
	assert.Equal(t, st.maxMsgSize, int64(128<<10))        // stricter bound kept
}

// The receive queue's byte budget is StreamWindow + MaxMessageSize -- the
// documented per-stream receive peak (a window of un-granted bytes plus one
// message overdrafting to finish) -- so the queue never blocks the shared reader
// under a well-behaved peer. With flow control off, no budget is installed and
// the queue stays slot-bounded.
func TestStream_RecvQueueByteBudget(t *testing.T) {
	opts := Options{SplitSize: 64 << 10}
	drpcopts.SetStreamFlowControl(&opts.Internal, drpcopts.FlowControl{
		Enabled: true, StreamWindow: 256 << 10, GrantThreshold: 64 << 10, MaxMessageSize: 128 << 10,
	})
	st := NewWithOptions(context.Background(), 1, testMuxWriter(t), NewBufferPool(), drpcmetrics.ConnectionMetrics{}, opts)
	assert.Equal(t, st.recvQueue.maxBytes, int64(256<<10)+int64(128<<10))

	// Flow control off: no byte budget, legacy slot bound.
	off := NewWithOptions(context.Background(), 1, testMuxWriter(t), NewBufferPool(), drpcmetrics.ConnectionMetrics{}, Options{SplitSize: 64 << 10})
	assert.Equal(t, off.recvQueue.maxBytes, int64(0))
}

// A message larger than the window completes by overdrafting once the sender has
// committed to it, instead of parking for credit that consume-driven flow control
// cannot return without a complete message. "hello" (5 bytes, frames 2+2+1)
// exceeds the 4-byte window and drives the balance to -1.
func TestStream_FlowControlOverdraftsToFinish(t *testing.T) {
	st := smallFCStream(t, 4, 16)
	assert.NoError(t, st.RawWrite(drpcwire.KindMessage, []byte("hello")))
	assert.Equal(t, st.sendw.available(), int64(-1)) // 4 - 5
}

// Gating happens at the message boundary, not per frame: with the window
// overdrawn by one message, the next message parks on its first frame until a
// grant repays the deficit.
func TestStream_FlowControlGatesNextMessage(t *testing.T) {
	st := smallFCStream(t, 4, 16)
	assert.NoError(t, st.RawWrite(drpcwire.KindMessage, []byte("hello"))) // avail -> -1

	done := make(chan error, 1)
	go func() { done <- st.RawWrite(drpcwire.KindMessage, []byte("x")) }()
	select {
	case <-done:
		t.Fatal("next message sent before credit was repaid")
	case <-time.After(blockShort):
	}

	// Repay the overdraft and cover the next frame; the parked send resumes.
	assert.NoError(t, st.HandleFrame(drpcwire.WindowUpdateFrame(st.ID(), 4)))
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("parked send did not resume after grant")
	}
}

// A message exactly at MaxMessageSize completes (via overdraft); one byte over
// fails fast with a MessageSizeError instead of overdrafting or parking.
func TestStream_FlowControlBoundaryAndOversized(t *testing.T) {
	st := smallFCStream(t, 4, 8)

	assert.NoError(t, st.RawWrite(drpcwire.KindMessage, make([]byte, 8))) // == bound: ok

	err := st.RawWrite(drpcwire.KindMessage, make([]byte, 9)) // > bound: fail fast
	assert.Error(t, err)
	assert.That(t, drpc.MessageSizeError.Has(err))
}

// An oversized incoming message fails only the receiving stream: HandleFrame
// returns nil (so the manager keeps the multiplexed connection alive) while the
// stream itself terminates and surfaces the size error to the reader.
func TestStream_OversizedRecvFailsStreamNotConnection(t *testing.T) {
	st := smallFCStream(t, 4, 8) // receive bound = 8 bytes

	fr := drpcwire.Frame{
		ID:   drpcwire.ID{Stream: st.ID(), Message: 1},
		Kind: drpcwire.KindMessage,
		Data: make([]byte, 9), // exceeds the 8-byte bound
		Done: true,
	}
	// Must not return an error: the manager treats a HandleFrame error as
	// connection-fatal, which would tear down unrelated RPCs.
	assert.NoError(t, st.HandleFrame(fr))

	// The stream is terminated and the reader sees the size error.
	_, err := st.RawRecv()
	assert.That(t, drpc.MessageSizeError.Has(err))
}

// Rejecting an oversized receive must notify the peer with an abortive terminal
// error, otherwise a credit-gated send there hangs forever. The KindError frame
// carries ResourceExhausted, and the connection is preserved (HandleFrame
// returns nil).
func TestStream_OversizedRecvNotifiesPeer(t *testing.T) {
	mw, frames := captureWriter(t)
	opts := Options{SplitSize: 2}
	drpcopts.SetStreamFlowControl(&opts.Internal, drpcopts.FlowControl{
		Enabled: true, StreamWindow: 4, GrantThreshold: 2, MaxMessageSize: 8,
	})
	st := NewWithOptions(context.Background(), 1, mw, NewBufferPool(), drpcmetrics.ConnectionMetrics{}, opts)

	fr := drpcwire.Frame{
		ID:   drpcwire.ID{Stream: st.ID(), Message: 1},
		Kind: drpcwire.KindMessage,
		Data: make([]byte, 9), // exceeds the 8-byte bound
		Done: true,
	}
	assert.NoError(t, st.HandleFrame(fr)) // connection preserved

	// A terminal frame reaches the wire (the counterpart to "no terminal frame
	// reaches the wire"), carrying ResourceExhausted for the peer.
	got := waitFrame(t, frames)
	assert.Equal(t, got.Kind, drpcwire.KindError)
	assert.Equal(t, status.Code(drpcwire.UnmarshalError(got.Data)), codes.ResourceExhausted)

	// The local reader still sees the size error.
	_, err := st.RawRecv()
	assert.That(t, drpc.MessageSizeError.Has(err))
}

// A sender that overruns the receive-window cap (only possible by ignoring flow
// control) fails only its own stream: HandleFrame keeps the connection alive (it
// never returns an error and never blocks), the peer gets an abortive
// ResourceExhausted, and the local reader sees the cap error after the already
// queued messages drain.
func TestStream_ReceiveCapOverrunFailsStream(t *testing.T) {
	mw, frames := captureWriter(t)
	opts := Options{SplitSize: 2}
	drpcopts.SetStreamFlowControl(&opts.Internal, drpcopts.FlowControl{
		Enabled: true, StreamWindow: 4, GrantThreshold: 2, MaxMessageSize: 4,
	})
	st := NewWithOptions(context.Background(), 1, mw, NewBufferPool(), drpcmetrics.ConnectionMetrics{}, opts)
	// Receive-queue cap = StreamWindow + MaxMessageSize = 8 bytes.

	// Two 4-byte messages fill the cap without being consumed; both are accepted
	// and HandleFrame returns promptly (the reader is never blocked).
	for mid := uint64(1); mid <= 2; mid++ {
		assert.NoError(t, st.HandleFrame(msgFrame(st.ID(), mid, make([]byte, 4), true)))
	}

	// The third 4-byte message overruns the 8-byte cap. The stream fails, but the
	// connection is preserved: HandleFrame returns nil rather than blocking.
	assert.NoError(t, st.HandleFrame(msgFrame(st.ID(), 3, make([]byte, 4), true)))

	// An abortive terminal frame reaches the peer with ResourceExhausted.
	got := waitFrame(t, frames)
	assert.Equal(t, got.Kind, drpcwire.KindError)
	assert.Equal(t, status.Code(drpcwire.UnmarshalError(got.Data)), codes.ResourceExhausted)

	// The two queued messages drain first, then the reader sees the cap error.
	for i := 0; i < 2; i++ {
		_, err := st.RawRecv()
		assert.NoError(t, err)
	}
	_, err := st.RawRecv()
	assert.That(t, drpc.ReceiveCapError.Has(err))
}
