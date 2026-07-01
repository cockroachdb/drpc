// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcmanager

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/zeebo/assert"

	"storj.io/drpc/drpcstream"
	"storj.io/drpc/drpctest"
	"storj.io/drpc/drpcwire"
)

// Enabling flow control through the manager's Stream options installs windows
// on every stream it creates (client and server), so a message larger than the
// send window only completes if credit grants flow across the real connection.
func TestManager_FlowControlEndToEnd(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	opts := Options{Stream: drpcstream.Options{
		SplitSize: 64 << 10,
		FlowControl: drpcstream.FlowControl{
			Enabled:        true,
			StreamWindow:   128 << 10, // 2 frames of initial credit
			HighWater:      1 << 20,
			GrantThreshold: 64 << 10,
		},
	}}

	cman := NewWithOptions(cconn, Client, opts)
	defer func() { _ = cman.Close() }()
	sman := NewWithOptions(sconn, Server, opts)
	defer func() { _ = sman.Close() }()

	// 256 KiB is four frames: two fit in the initial window, the rest can only
	// be sent as the server dispatches frames and returns credit.
	msg := make([]byte, 256<<10)

	ctx.Run(func(ctx context.Context) {
		stream, err := cman.NewClientStream(ctx, "rpc")
		assert.NoError(t, err)
		assert.NoError(t, stream.RawWrite(drpcwire.KindInvoke, []byte("rpc")))
		assert.NoError(t, stream.RawWrite(drpcwire.KindMessage, msg))
		assert.NoError(t, stream.Close())
	})

	ctx.Run(func(ctx context.Context) {
		stream, _, err := sman.NewServerStream(ctx)
		assert.NoError(t, err)
		defer func() { _ = stream.Close() }()

		got, err := stream.RawRecv()
		assert.NoError(t, err)
		assert.Equal(t, len(got), len(msg))

		_, err = stream.RawRecv()
		assert.That(t, errors.Is(err, io.EOF))
	})

	ctx.Wait()
}

// Cancelling an RPC's context wakes a sender parked on flow-control credit. The
// client has flow control with a small window; the server does not, so it never
// returns credit and the client's oversized send parks. This exercises the
// production wake path: manageStream sees the cancellation and calls
// Cancel -> terminate -> sendWindow.close.
func TestManager_FlowControlContextCancelWakesParkedSend(t *testing.T) {
	tr := drpctest.NewTracker(t)
	defer tr.Close()

	cconn, sconn := net.Pipe()
	defer func() { _ = cconn.Close() }()
	defer func() { _ = sconn.Close() }()

	cman := NewWithOptions(cconn, Client, Options{Stream: drpcstream.Options{
		SplitSize: 64 << 10,
		FlowControl: drpcstream.FlowControl{
			Enabled:        true,
			StreamWindow:   128 << 10, // two frames of initial credit
			HighWater:      1 << 20,
			GrantThreshold: 64 << 10,
		},
	}})
	defer func() { _ = cman.Close() }()
	// Server has no flow control, so it never returns credit.
	sman := New(sconn, Server)
	defer func() { _ = sman.Close() }()

	// Accept the stream and drain; the message never completes (the sender
	// parks mid-message), so RawRecv just blocks until teardown.
	tr.Run(func(ctx context.Context) {
		stream, _, err := sman.NewServerStream(ctx)
		if err != nil {
			return
		}
		defer func() { _ = stream.Close() }()
		for {
			if _, err := stream.RawRecv(); err != nil {
				return
			}
		}
	})

	streamCtx, cancel := context.WithCancel(context.Background())
	stream, err := cman.NewClientStream(streamCtx, "rpc")
	assert.NoError(t, err)
	assert.NoError(t, stream.RawWrite(drpcwire.KindInvoke, []byte("rpc")))

	// A 512 KiB message exceeds the 128 KiB window; once the initial credit is
	// spent the send parks waiting for grants that never come.
	done := make(chan error, 1)
	go func() { done <- stream.RawWrite(drpcwire.KindMessage, make([]byte, 512<<10)) }()

	select {
	case <-done:
		t.Fatal("send returned before it could park on credit")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()

	select {
	case err := <-done:
		assert.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("parked send did not wake on context cancellation")
	}
}
