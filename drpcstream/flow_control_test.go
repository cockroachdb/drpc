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
func fcOptions(window, threshold int64) Options {
	opts := Options{SplitSize: 64 << 10}
	drpcopts.SetStreamFlowControl(&opts.Internal, drpcopts.FlowControl{
		Enabled:        true,
		StreamWindow:   window,
		GrantThreshold: threshold,
	})
	return opts
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
		t.Helper()
		st := NewWithOptions(context.Background(), 1, testMuxWriter(t), NewBufferPool(), drpcmetrics.ConnectionMetrics{}, opts)
		assert.Equal(t, st.sendw.available(), defaultWindow)
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
	// wire. The rest of each config is valid, so the window is kept.
	def := fcOptions(128<<10, 64<<10)
	def.SplitSize = 0 // unset
	st := NewWithOptions(context.Background(), 1, testMuxWriter(t), NewBufferPool(), drpcmetrics.ConnectionMetrics{}, def)
	assert.Equal(t, st.sendw.available(), int64(128<<10))         // valid: kept
	assert.Equal(t, st.opts.SplitSize, drpcwire.DefaultFrameSize) // defaulted

	unbounded := fcOptions(256<<10, 64<<10)
	unbounded.SplitSize = -1 // unbounded frames
	st = NewWithOptions(context.Background(), 1, testMuxWriter(t), NewBufferPool(), drpcmetrics.ConnectionMetrics{}, unbounded)
	assert.Equal(t, st.sendw.available(), int64(256<<10))         // valid: kept
	assert.Equal(t, st.opts.SplitSize, drpcwire.DefaultFrameSize) // defaulted from unbounded

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

// End to end via the option: an option-installed window gates a data write
// that exceeds the initial credit, and an incoming grant resumes it. SplitSize
// 2 with a 4-byte window means "hello" (frames 2+2+1) parks on its last frame.
func TestStream_FlowControlOptionGatesAndResumes(t *testing.T) {
	mw := testMuxWriter(t)
	opts := Options{SplitSize: 2}
	drpcopts.SetStreamFlowControl(&opts.Internal, drpcopts.FlowControl{
		Enabled: true, StreamWindow: 4, GrantThreshold: 2,
	})
	st := NewWithOptions(context.Background(), 1, mw, NewBufferPool(), drpcmetrics.ConnectionMetrics{}, opts)

	done := make(chan error, 1)
	go func() { done <- st.RawWrite(drpcwire.KindMessage, []byte("hello")) }() // 5 bytes > 4

	select {
	case <-done:
		t.Fatal("write returned before sufficient credit")
	case <-time.After(blockShort):
	}

	// A grant from the peer tops up the send window and resumes the write.
	assert.NoError(t, st.HandleFrame(drpcwire.WindowUpdateFrame(st.ID(), 1)))

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("write did not resume after grant")
	}
}
