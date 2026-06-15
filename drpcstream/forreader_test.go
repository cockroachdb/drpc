// Copyright (C) 2026 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcstream

import (
	"context"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"storj.io/drpc/drpcwire"
)

// tcpPair returns a connected pair of net.Conn over loopback TCP. Unlike
// net.Pipe (which is synchronous and would deadlock when drpc's Close writes a
// KindClose frame with no concurrent reader), TCP conns are buffered/async.
func tcpPair(t *testing.T) (server, client net.Conn, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()

	client, err = net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	r := <-ch
	require.NoError(t, r.err)
	server = r.c

	return server, client, func() {
		_ = server.Close()
		_ = client.Close()
		_ = ln.Close()
	}
}

func newForReaderStream(ctx context.Context, tr net.Conn) *Stream {
	return NewForReader(ctx, 1, tr, drpcwire.NewReader(tr), drpcwire.NewWriter(tr, 0), Options{})
}

// readLoop feeds HandlePacket so a message sent by the peer is received.
func TestNewForReader_Recv(t *testing.T) {
	a, b, cleanup := tcpPair(t)
	defer cleanup()
	ctx := context.Background()

	srv := newForReaderStream(ctx, a)
	cli := newForReaderStream(ctx, b)

	require.NoError(t, cli.MsgSend([]byte("hello"), byteEncoding{}))

	var out []byte
	require.NoError(t, srv.MsgRecv(&out, byteEncoding{}))
	require.Equal(t, "hello", string(out))

	_ = srv.Close()
	_ = cli.Close()
}

// Finishing the stream closes its transport and lets the readLoop + watcher
// goroutines exit (no leak).
func TestNewForReader_FinishReleasesGoroutines(t *testing.T) {
	a, b, cleanup := tcpPair(t)
	defer cleanup()
	ctx := context.Background()

	runtime.GC()
	base := runtime.NumGoroutine()

	// Peer (b) never sends, so the readLoop blocks in Read until Close.
	srv := newForReaderStream(ctx, a)
	require.NoError(t, srv.Close())

	require.Eventually(t, func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= base+1
	}, 3*time.Second, 20*time.Millisecond, "readLoop/watcher goroutines did not exit")

	_ = b // keep the peer conn alive until cleanup
}

// Canceling the parent context terminates the stream and unblocks operations.
func TestNewForReader_CancelTerminates(t *testing.T) {
	a, b, cleanup := tcpPair(t)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())

	srv := newForReaderStream(ctx, a)

	cancel() // watcher: ctx.Done -> s.Cancel -> terminate -> close transport

	require.Eventually(t, srv.IsFinished, 3*time.Second, 20*time.Millisecond,
		"stream did not finish after ctx cancel")

	var out []byte
	require.Error(t, srv.MsgRecv(&out, byteEncoding{})) // returns, does not hang
	_ = b
}
