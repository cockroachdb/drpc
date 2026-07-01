// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcmanager

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/zeebo/assert"

	"storj.io/drpc"
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
