// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import (
	"context"
	"testing"
	"time"

	"github.com/zeebo/assert"
	"github.com/zeebo/errs"

	"storj.io/drpc/drpcwire"
)

// newGateStream builds a stream writing to io.Discard with an explicit
// SplitSize so small payloads are a single frame.
func newGateStream(t *testing.T) *Stream {
	mw := testMuxWriter(t)
	return NewWithOptions(context.Background(), 1, mw, NewBufferPool(), Options{SplitSize: 64 << 10})
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
	st.sendw = newSendWindow(4) // 4 bytes of credit

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
	st.sendw = newSendWindow(0) // no credit at all

	assert.NoError(t, st.WriteInvoke("service.Method", nil))
}

// Terminating the stream wakes a send parked on credit.
func TestStream_SendWindowTerminateWakesParkedWrite(t *testing.T) {
	st := newGateStream(t)
	st.sendw = newSendWindow(0) // send will park immediately

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
		assert.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("parked data write did not wake on termination")
	}
}
