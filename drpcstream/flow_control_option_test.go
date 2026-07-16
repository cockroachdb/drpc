// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/zeebo/assert"

	"storj.io/drpc/drpcwire"
	"storj.io/drpc/internal/drpcopts"
)

// fcOptions returns stream Options with flow control enabled via the internal
// option, the only way to enable it until it is promoted to a public option.
func fcOptions(window, highWater, threshold int64) Options {
	opts := Options{SplitSize: 64 << 10}
	drpcopts.SetStreamFlowControl(&opts.Internal, drpcopts.FlowControl{
		Enabled:        true,
		StreamWindow:   window,
		HighWater:      highWater,
		GrantThreshold: threshold,
	})
	return opts
}

// Enabling flow control installs the send and receive windows, seeding the
// send window with the configured stream window as initial credit.
func TestStream_FlowControlEnabledInstallsWindows(t *testing.T) {
	mw := testMuxWriter(t)
	st := NewWithOptions(context.Background(), 1, mw, NewBufferPool(), fcOptions(256<<10, 4<<20, 128<<10))
	assert.That(t, st.sendw != nil)
	assert.That(t, st.recvw != nil)
	assert.Equal(t, st.sendw.available(), int64(256<<10))
}

// Without flow control enabled, no windows are installed and behavior is
// unchanged (ungated).
func TestStream_FlowControlDisabledNoWindows(t *testing.T) {
	mw := testMuxWriter(t)
	st := NewWithOptions(context.Background(), 1, mw, NewBufferPool(), Options{SplitSize: 64 << 10})
	assert.That(t, st.sendw == nil)
	assert.That(t, st.recvw == nil)
}

// An invalid flow-control configuration panics at construction rather than
// deadlocking later or silently disabling the protection.
func TestStream_FlowControlValidation(t *testing.T) {
	mustPanic := func(name string, opts Options) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Fatalf("%s: expected construction to panic", name)
			}
		}()
		NewWithOptions(context.Background(), 1, testMuxWriter(t), NewBufferPool(), opts)
	}

	mustPanic("zero window", fcOptions(0, 4<<20, 128<<10))
	mustPanic("zero high water", fcOptions(256<<10, 0, 128<<10))
	mustPanic("zero threshold", fcOptions(256<<10, 4<<20, 0))
	mustPanic("negative window", fcOptions(-1, 4<<20, 128<<10))

	// Threshold + frame must fit in the window, or a quiescent receiver can
	// strand the sender below the next frame's cost.
	mustPanic("threshold+frame exceeds window", fcOptions(128<<10, 4<<20, 128<<10))

	// A near-max threshold must not sneak past via int64 overflow of the
	// threshold+frame sum.
	mustPanic("near-max threshold overflow", fcOptions(256<<10, 4<<20, math.MaxInt64))

	// SplitSize < 0 means unsplit (unbounded) frames: no bound holds.
	unbounded := fcOptions(256<<10, 4<<20, 64<<10)
	unbounded.SplitSize = -1
	mustPanic("unbounded SplitSize", unbounded)

	// SplitSize 0 uses SplitData's 64 KiB default as the frame size.
	def := fcOptions(128<<10, 4<<20, 64<<10)
	def.SplitSize = 0
	NewWithOptions(context.Background(), 1, testMuxWriter(t), NewBufferPool(), def) // 64K+64K <= 128K: ok
	def2 := fcOptions(96<<10, 4<<20, 64<<10)
	def2.SplitSize = 0
	mustPanic("default frame exceeds window headroom", def2)
}

// End to end via the option: an option-installed window gates a data write
// that exceeds the initial credit, and an incoming grant resumes it. SplitSize
// 2 with a 4-byte window means "hello" (frames 2+2+1) parks on its last frame.
func TestStream_FlowControlOptionGatesAndResumes(t *testing.T) {
	mw := testMuxWriter(t)
	opts := Options{SplitSize: 2}
	drpcopts.SetStreamFlowControl(&opts.Internal, drpcopts.FlowControl{
		Enabled: true, StreamWindow: 4, HighWater: 1 << 20, GrantThreshold: 2,
	})
	st := NewWithOptions(context.Background(), 1, mw, NewBufferPool(), opts)

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
