// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zeebo/assert"

	"storj.io/drpc/drpcwire"
)

// Enabling flow control via Options installs the send and receive windows,
// seeding the send window with the configured stream window as initial credit.
func TestStream_FlowControlEnabledInstallsWindows(t *testing.T) {
	mw := testMuxWriter(t)
	st := NewWithOptions(context.Background(), 1, mw, NewBufferPool(), Options{
		SplitSize: 64 << 10,
		FlowControl: FlowControl{
			Enabled:        true,
			StreamWindow:   256 << 10,
			HighWater:      4 << 20,
			GrantThreshold: 128 << 10,
		},
	})
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

// End to end via the option: an option-installed window gates a data write that
// exceeds the initial credit, and an incoming grant resumes it.
func TestStream_FlowControlOptionGatesAndResumes(t *testing.T) {
	mw := testMuxWriter(t)
	st := NewWithOptions(context.Background(), 1, mw, NewBufferPool(), Options{
		SplitSize:   64 << 10,
		FlowControl: FlowControl{Enabled: true, StreamWindow: 4, HighWater: 1 << 20, GrantThreshold: 2},
	})

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

// A message larger than the implicit maximum (high_water + window) is rejected
// up front rather than parking the sender on a message that can never complete.
func TestStream_FlowControlRejectsOversizedMessage(t *testing.T) {
	mw := testMuxWriter(t)
	st := NewWithOptions(context.Background(), 1, mw, NewBufferPool(), Options{
		SplitSize:   64 << 10,
		FlowControl: FlowControl{Enabled: true, StreamWindow: 256, HighWater: 1024, GrantThreshold: 128},
	})
	// maxMsg = HighWater + StreamWindow = 1280.
	done := make(chan error, 1)
	go func() { done <- st.RawWrite(drpcwire.KindMessage, make([]byte, 1281)) }()

	select {
	case err := <-done:
		assert.Error(t, err)
		assert.That(t, strings.Contains(err.Error(), "exceeds"))
	case <-time.After(time.Second):
		t.Fatal("oversized message did not fail fast (it parked on credit instead)")
	}
}

// With flow control disabled there is no implicit message-size limit.
func TestStream_NoFlowControlNoMessageSizeLimit(t *testing.T) {
	mw := testMuxWriter(t)
	st := NewWithOptions(context.Background(), 1, mw, NewBufferPool(), Options{SplitSize: 64 << 10})
	assert.NoError(t, st.RawWrite(drpcwire.KindMessage, make([]byte, 256<<10)))
}
