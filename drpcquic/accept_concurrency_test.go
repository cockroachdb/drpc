// Copyright (C) 2026 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcquic

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"storj.io/drpc/drpcconn"
	"storj.io/drpc/drpcmetadata"
	"storj.io/drpc/drpcwire"
)

// A stream that opens but stalls before sending its invoke must NOT block other
// streams on the same connection from being accepted and served. This guards the
// server accept loop against head-of-line blocking on the invoke read: accepting
// (and serving) one stream must not wait for another stream's invoke to arrive.
//
// On the buggy version (accept + invoke-read done together in the accept loop)
// the second RPC hangs and this test fails via timeout.
func TestServe_SlowInvokeDoesNotBlockOtherStreams(t *testing.T) {
	addr, clientTLS, stop := startServer(t, echoHandler{}, Options{})
	defer stop()

	ctx := context.Background()
	mt, err := Dial(ctx, addr, clientTLS, Options{})
	require.NoError(t, err)
	defer func() { _ = mt.Close() }()

	// Open a stream and send ONLY a metadata frame (a well-formed prefix), never
	// the invoke. The server accepts this stream and then blocks reading its
	// invoke. Flushed first so it is the lower-numbered stream the server accepts
	// before the RPC stream below.
	slow, err := mt.OpenStream(ctx)
	require.NoError(t, err)
	md, err := drpcmetadata.Encode(nil, map[string]string{"stall": "yes"})
	require.NoError(t, err)
	w := drpcwire.NewWriter(slow, 0)
	require.NoError(t, w.WritePacket(drpcwire.Packet{
		Kind: drpcwire.KindInvokeMetadata,
		ID:   drpcwire.ID{Stream: 1, Message: 1},
		Data: md,
	}))
	require.NoError(t, w.Flush())

	// Give the server a moment to accept the slow stream and enter its invoke
	// read, so that on the buggy version the accept loop is provably stuck there.
	time.Sleep(100 * time.Millisecond)

	// On the SAME connection, a normal unary RPC must still complete promptly.
	conn := drpcconn.NewFromMultiplexed(mt, drpcconn.Options{})

	done := make(chan error, 1)
	go func() {
		out := new(strMsg)
		e := conn.Invoke(ctx, "/echo", strEnc{}, &strMsg{S: "hi"}, out)
		if e == nil && out.S != "echo:hi" {
			e = fmt.Errorf("bad echo: got %q want echo:hi", out.S)
		}
		done <- e
	}()

	select {
	case e := <-done:
		require.NoError(t, e)
	case <-time.After(5 * time.Second):
		t.Fatal("Invoke on a second stream hung: the accept loop serialized on the slow stream's invoke read")
	}
}

// Shutting the server down while a stream is parked mid-invoke-read must not
// hang. The per-stream goroutine's deadline-less read only unblocks when the
// connection is closed, so the server's drain must Close before it Waits. With
// the wrong shutdown order this test hangs (Wait blocks forever) and fails via
// timeout.
func TestServe_ShutdownWithStalledInvokeDoesNotHang(t *testing.T) {
	addr, clientTLS, stop := startServer(t, echoHandler{}, Options{})

	ctx := context.Background()
	mt, err := Dial(ctx, addr, clientTLS, Options{})
	require.NoError(t, err)
	defer func() { _ = mt.Close() }()

	// Open a stream that sends metadata but never the invoke, parking a server
	// per-stream goroutine in the invoke read.
	slow, err := mt.OpenStream(ctx)
	require.NoError(t, err)
	md, err := drpcmetadata.Encode(nil, map[string]string{"stall": "yes"})
	require.NoError(t, err)
	w := drpcwire.NewWriter(slow, 0)
	require.NoError(t, w.WritePacket(drpcwire.Packet{
		Kind: drpcwire.KindInvokeMetadata,
		ID:   drpcwire.ID{Stream: 1, Message: 1},
		Data: md,
	}))
	require.NoError(t, w.Flush())
	time.Sleep(100 * time.Millisecond) // let the server park in the invoke read

	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("server shutdown hung on a stream stalled mid-invoke (Wait ran before Close)")
	}
}
