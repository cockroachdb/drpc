// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import (
	"context"
	"errors"
	"io"
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

// SendCancel preempts a send parked on credit: terminate (which closes the
// window) runs before SendCancel takes the write lock, so the parked write
// wakes, releases the lock, and the cancel frame goes out.
func TestStream_SendWindowSendCancelPreemptsParkedWrite(t *testing.T) {
	st := newGateStream(t)
	st.sendw = newSendWindow(0) // send will park immediately

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

// Close preempts a send parked on credit the same way: it closes the send
// window before taking the write lock rather than waiting on a grant that
// may never come.
func TestStream_SendWindowClosePreemptsParkedWrite(t *testing.T) {
	st := newGateStream(t)
	st.sendw = newSendWindow(0) // send will park immediately

	done := make(chan error, 1)
	go func() { done <- st.RawWrite(drpcwire.KindMessage, []byte("data")) }()

	select {
	case <-done:
		t.Fatal("data write returned before close")
	case <-time.After(blockShort):
	}

	assert.NoError(t, st.Close())

	select {
	case err := <-done:
		// Same error later sends see: terminate sets sigs.send to termClosed.
		assert.That(t, errors.Is(err, termClosed))
	case <-time.After(time.Second):
		t.Fatal("parked data write was not preempted by Close")
	}
}

// SendError preempts a send parked on credit, like Close: it closes the send
// window before taking the write lock so reporting an error is never stuck
// behind a slow consumer.
func TestStream_SendWindowSendErrorPreemptsParkedWrite(t *testing.T) {
	st := newGateStream(t)
	st.sendw = newSendWindow(0) // send will park immediately

	done := make(chan error, 1)
	go func() { done <- st.RawWrite(drpcwire.KindMessage, []byte("data")) }()

	select {
	case <-done:
		t.Fatal("data write returned before error")
	case <-time.After(blockShort):
	}

	assert.NoError(t, st.SendError(errs.New("boom")))

	select {
	case err := <-done:
		// io.EOF, matching sigs.send: parked and later sends agree.
		assert.That(t, errors.Is(err, io.EOF))
	case <-time.After(time.Second):
		t.Fatal("parked data write was not preempted by SendError")
	}
}

// CloseSend is a graceful half-close, not a termination: it must NOT preempt
// a send parked on credit. It waits for the write lock; once credit arrives
// the parked write completes successfully and CloseSend follows it out.
func TestStream_SendWindowCloseSendWaitsForParkedWrite(t *testing.T) {
	st := newGateStream(t)
	st.sendw = newSendWindow(0) // send will park immediately

	write := make(chan error, 1)
	go func() { write <- st.RawWrite(drpcwire.KindMessage, []byte("data")) }()

	select {
	case <-write:
		t.Fatal("data write returned before credit")
	case <-time.After(blockShort):
	}

	closeSend := make(chan error, 1)
	go func() { closeSend <- st.CloseSend() }()

	// Neither may make progress yet: the write is parked on credit and
	// CloseSend is parked behind it on the write lock.
	select {
	case <-write:
		t.Fatal("data write returned without credit")
	case <-closeSend:
		t.Fatal("CloseSend preempted a parked data write")
	case <-time.After(blockShort):
	}

	st.sendw.grant(uint64(len("data")))

	select {
	case err := <-write:
		assert.NoError(t, err) // the parked write completed, not aborted
	case <-time.After(time.Second):
		t.Fatal("parked data write did not complete after grant")
	}
	select {
	case err := <-closeSend:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("CloseSend did not complete after the parked write finished")
	}
}

// While CloseSend waits behind a credit-parked send, termination must still
// be able to proceed: Cancel needs s.mu to terminate, so CloseSend must not
// hold s.mu while waiting for the write lock.
func TestStream_SendWindowCancelUnwedgesCloseSendBehindParkedWrite(t *testing.T) {
	st := newGateStream(t)
	st.sendw = newSendWindow(0) // send will park immediately

	write := make(chan error, 1)
	go func() { write <- st.RawWrite(drpcwire.KindMessage, []byte("data")) }()

	select {
	case <-write:
		t.Fatal("data write returned before credit")
	case <-time.After(blockShort):
	}

	closeSend := make(chan error, 1)
	go func() { closeSend <- st.CloseSend() }()

	select {
	case <-closeSend:
		t.Fatal("CloseSend preempted a parked data write")
	case <-time.After(blockShort):
	}

	// Cancel terminates the stream, closing the send window: the parked write
	// wakes with an error and CloseSend unblocks as a no-op.
	canceled := make(chan struct{})
	go func() { st.Cancel(errs.New("boom")); close(canceled) }()

	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("Cancel blocked behind CloseSend waiting for the write lock")
	}
	select {
	case err := <-write:
		assert.That(t, errors.Is(err, io.EOF))
	case <-time.After(time.Second):
		t.Fatal("parked data write did not wake on termination")
	}
	select {
	case err := <-closeSend:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("CloseSend did not unblock after termination")
	}
}

// CloseSend acquires the write lock before s.mu; Close and SendError must
// use the same order (never holding s.mu while waiting for the write lock),
// or the two paths ABBA-deadlock racing behind a credit-parked send.
func TestStream_SendWindowCloseSendVsTerminatorNoDeadlock(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Stream) error
	}{
		{"Close", func(st *Stream) error { return st.Close() }},
		{"SendError", func(st *Stream) error { return st.SendError(errs.New("boom")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newGateStream(t)
			st.sendw = newSendWindow(0) // send will park immediately

			write := make(chan error, 1)
			go func() { write <- st.RawWrite(drpcwire.KindMessage, []byte("data")) }()

			select {
			case <-write:
				t.Fatal("data write returned before credit")
			case <-time.After(blockShort):
			}

			closeSend := make(chan error, 1)
			terminator := make(chan error, 1)
			go func() { closeSend <- st.CloseSend() }()
			go func() { terminator <- tc.call(st) }()

			// All three must resolve; the interesting interleaving is CloseSend
			// winning the write lock while the terminator wins s.mu.
			for name, ch := range map[string]chan error{
				"parked write": write, "CloseSend": closeSend, tc.name: terminator,
			} {
				select {
				case <-ch:
				case <-time.After(2 * time.Second):
					t.Fatalf("%s deadlocked", name)
				}
			}
		})
	}
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
		// Cancel pre-sets sigs.send to io.EOF; the window closes with it.
		assert.That(t, errors.Is(err, io.EOF))
	case <-time.After(time.Second):
		t.Fatal("parked data write did not wake on termination")
	}
}
