// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcmanager

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"

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
