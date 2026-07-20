// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcmanager

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/zeebo/assert"

	"storj.io/drpc"
	"storj.io/drpc/drpcstream"
	"storj.io/drpc/drpctest"
	"storj.io/drpc/drpcwire"
)

// A server with MaxStreams set refuses an inbound stream beyond the cap with a
// stream-level error, without tearing down the connection or admitting the
// stream.
func TestManager_MaxStreamsRefusesExcessInbound(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	cman := New(cconn, Client)
	defer func() { _ = cman.Close() }()
	sman := NewWithOptions(sconn, Server, Options{MaxStreams: 1})
	defer func() { _ = sman.Close() }()

	accepted := make(chan struct{})
	release := make(chan struct{})

	// Server accepts exactly one stream and holds it open (so it keeps counting
	// toward the cap) until released.
	ctx.Run(func(ctx context.Context) {
		s1, _, err := sman.NewServerStream(ctx)
		assert.NoError(t, err)
		close(accepted)
		<-release
		_ = s1.Close()
	})

	ctx.Run(func(ctx context.Context) {
		// First stream is admitted.
		c1, err := cman.NewClientStream(ctx, "rpc", drpc.CompressionNone)
		assert.NoError(t, err)
		assert.NoError(t, c1.RawWrite(drpcwire.KindInvoke, []byte("rpc")))
		<-accepted // ensure the first stream is active before opening the second

		// Second stream: the server is at its cap, so it is refused with a
		// stream-level error surfaced on the next receive.
		c2, err := cman.NewClientStream(ctx, "rpc", drpc.CompressionNone)
		assert.NoError(t, err)
		assert.NoError(t, c2.RawWrite(drpcwire.KindInvoke, []byte("rpc")))
		_, err = c2.RawRecv()
		assert.Error(t, err)
		assert.That(t, strings.Contains(err.Error(), "concurrent streams"))

		close(release)
		_ = c1.Close()
	})

	ctx.Wait()
}

// writeRawFrame marshals and writes a single frame to conn, as a raw peer.
func writeRawFrame(t *testing.T, conn net.Conn, fr drpcwire.Frame) {
	t.Helper()
	if _, err := conn.Write(drpcwire.AppendFrame(nil, fr)); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

// readErrorForStream reads frames from conn until it sees a KindError for sid,
// returning the decoded error. Frames for other streams are skipped.
func readErrorForStream(t *testing.T, rd *drpcwire.Reader, sid uint64) error {
	t.Helper()
	for {
		fr, err := rd.ReadFrame()
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if fr.Kind == drpcwire.KindError && fr.ID.Stream == sid {
			return drpcwire.UnmarshalError(fr.Data)
		}
	}
}

// Streams still in setup (partial invoke, no completing frame) count against
// MaxStreams: a peer cannot hold unbounded half-open streams even though none
// are admitted.
func TestManager_MaxStreamsCountsPendingSetup(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	const max = 2
	sman := NewWithOptions(sconn, Server, Options{MaxStreams: max})
	defer func() { _ = sman.Close() }()

	rd := drpcwire.NewReader(cconn)

	// Open `max` streams but never complete their invokes (Done=false), so they
	// stay pending: none are admitted, yet all the slots are held.
	for sid := uint64(1); sid <= max; sid++ {
		writeRawFrame(t, cconn, drpcwire.Frame{
			ID:   drpcwire.ID{Stream: sid, Message: 1},
			Kind: drpcwire.KindInvoke,
			Data: []byte("rpc"),
			Done: false,
		})
	}

	// One more stream is refused, though streams.Len() is still 0.
	writeRawFrame(t, cconn, drpcwire.Frame{
		ID:   drpcwire.ID{Stream: max + 1, Message: 1},
		Kind: drpcwire.KindInvoke,
		Data: []byte("rpc"),
		Done: false,
	})

	err := readErrorForStream(t, rd, max+1)
	assert.Error(t, err)
	assert.That(t, strings.Contains(err.Error(), "concurrent streams"))
}

// A stream refused for exceeding MaxStreams does not tear down the connection:
// once a slot frees, a new stream is admitted.
func TestManager_MaxStreamsSlotFreesAfterClose(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	cman := New(cconn, Client)
	defer func() { _ = cman.Close() }()
	sman := NewWithOptions(sconn, Server, Options{MaxStreams: 1})
	defer func() { _ = sman.Close() }()

	served := make(chan *drpcstream.Stream, 2)
	ctx.Run(func(ctx context.Context) {
		for {
			s, _, err := sman.NewServerStream(ctx)
			if err != nil {
				return
			}
			served <- s
		}
	})

	// First stream is admitted; take and hold it.
	c1, err := cman.NewClientStream(ctx, "rpc", drpc.CompressionNone)
	assert.NoError(t, err)
	assert.NoError(t, c1.RawWrite(drpcwire.KindInvoke, []byte("rpc")))
	s1 := <-served

	// Close it, freeing the single slot, then open another: it is admitted.
	assert.NoError(t, s1.Close())
	assert.NoError(t, c1.Close())

	c2, err := cman.NewClientStream(ctx, "rpc", drpc.CompressionNone)
	assert.NoError(t, err)
	assert.NoError(t, c2.RawWrite(drpcwire.KindInvoke, []byte("rpc")))
	select {
	case <-served: // admitted: the slot was freed
	case <-time.After(time.Second):
		t.Fatal("second stream was not admitted after the first closed")
	}
	_ = c2.Close()
}

// With MaxStreams unset (0), the cap is disabled and many concurrent inbound
// streams are admitted.
func TestManager_MaxStreamsZeroUnlimited(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	cman := New(cconn, Client)
	defer func() { _ = cman.Close() }()
	sman := NewWithOptions(sconn, Server, Options{}) // MaxStreams 0
	defer func() { _ = sman.Close() }()

	const n = 8
	admitted := make(chan struct{}, n)
	ctx.Run(func(ctx context.Context) {
		for {
			s, _, err := sman.NewServerStream(ctx)
			if err != nil {
				return
			}
			admitted <- struct{}{}
			defer func() { _ = s.Close() }()
		}
	})

	streams := make([]*drpcstream.Stream, 0, n)
	for i := 0; i < n; i++ {
		c, err := cman.NewClientStream(ctx, "rpc", drpc.CompressionNone)
		assert.NoError(t, err)
		assert.NoError(t, c.RawWrite(drpcwire.KindInvoke, []byte("rpc")))
		streams = append(streams, c)
	}
	for i := 0; i < n; i++ {
		select {
		case <-admitted:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d streams admitted", i, n)
		}
	}
	for _, c := range streams {
		_ = c.Close()
	}
}

// The total invoke/metadata payload buffered during setup is bounded across
// continuation frames: a stream that accumulates more than
// MaxControlPayloadSize is refused, and the connection stays up.
func TestManager_MaxControlPayloadRefusesOversizedSetup(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	sman := NewWithOptions(sconn, Server, Options{MaxControlPayloadSize: 8})
	defer func() { _ = sman.Close() }()

	served := make(chan *drpcstream.Stream, 1)
	ctx.Run(func(ctx context.Context) {
		for {
			s, _, err := sman.NewServerStream(ctx)
			if err != nil {
				return
			}
			served <- s
		}
	})

	rd := drpcwire.NewReader(cconn)

	// Stream 1: two 5-byte continuation frames total 10 > 8, so the second
	// frame trips the cap even though neither frame alone exceeds it.
	writeRawFrame(t, cconn, drpcwire.Frame{
		ID: drpcwire.ID{Stream: 1, Message: 1}, Kind: drpcwire.KindInvoke,
		Data: []byte("aaaaa"), Done: false,
	})
	writeRawFrame(t, cconn, drpcwire.Frame{
		ID: drpcwire.ID{Stream: 1, Message: 1}, Kind: drpcwire.KindInvoke,
		Data: []byte("bbbbb"), Done: true,
	})
	err := readErrorForStream(t, rd, 1)
	assert.Error(t, err)
	assert.That(t, strings.Contains(err.Error(), "payload"))

	// The connection is still usable: a within-limit stream is admitted.
	writeRawFrame(t, cconn, drpcwire.Frame{
		ID: drpcwire.ID{Stream: 2, Message: 1}, Kind: drpcwire.KindInvoke,
		Data: []byte("rpc"), Done: true,
	})
	select {
	case s := <-served:
		_ = s.Close()
	case <-time.After(time.Second):
		t.Fatal("connection unusable after a control-payload rejection")
	}
}
