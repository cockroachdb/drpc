// Copyright (C) 2026 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcquic

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"storj.io/drpc/drpcconn"
)

// Bidirectional streaming: send/recv N, then CloseSend yields io.EOF.
func TestConn_Streaming(t *testing.T) {
	addr, clientTLS, stop := startServer(t, echoHandler{}, Options{})
	defer stop()

	ctx := context.Background()
	mt, err := Dial(ctx, addr, clientTLS, Options{})
	require.NoError(t, err)
	conn := drpcconn.NewFromMultiplexed(mt, drpcconn.Options{})
	defer func() { _ = conn.Close() }()

	stream, err := conn.NewStream(ctx, "/stream", strEnc{})
	require.NoError(t, err)

	for _, m := range []string{"a", "b", "c"} {
		require.NoError(t, stream.MsgSend(&strMsg{S: m}, strEnc{}))
		out := new(strMsg)
		require.NoError(t, stream.MsgRecv(out, strEnc{}))
		require.Equal(t, "echo:"+m, out.S)
	}

	require.NoError(t, stream.CloseSend())
	require.ErrorIs(t, stream.MsgRecv(new(strMsg), strEnc{}), io.EOF)
	require.NoError(t, stream.Close())
}

// Many concurrent unary Invokes on one connection all succeed — native
// multiplexing at scale.
func TestConn_ConcurrentFanout(t *testing.T) {
	addr, clientTLS, stop := startServer(t, echoHandler{}, Options{})
	defer stop()

	ctx := context.Background()
	mt, err := Dial(ctx, addr, clientTLS, Options{})
	require.NoError(t, err)
	conn := drpcconn.NewFromMultiplexed(mt, drpcconn.Options{})
	defer func() { _ = conn.Close() }()

	const n = 100
	errCh := make(chan error, n)
	for i := range n {
		go func(i int) {
			out := new(strMsg)
			want := fmt.Sprintf("m%d", i)
			e := conn.Invoke(ctx, "/echo", strEnc{}, &strMsg{S: want}, out)
			if e == nil && out.S != "echo:"+want {
				e = fmt.Errorf("bad echo: got %q want echo:%s", out.S, want)
			}
			errCh <- e
		}(i)
	}
	for range n {
		require.NoError(t, <-errCh)
	}
}

// Canceling the context mid-stream unblocks operations promptly (no hang).
func TestConn_ContextCancelMidStream(t *testing.T) {
	addr, clientTLS, stop := startServer(t, echoHandler{}, Options{})
	defer stop()

	mt, err := Dial(context.Background(), addr, clientTLS, Options{})
	require.NoError(t, err)
	conn := drpcconn.NewFromMultiplexed(mt, drpcconn.Options{})
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := conn.NewStream(ctx, "/stream", strEnc{})
	require.NoError(t, err)

	require.NoError(t, stream.MsgSend(&strMsg{S: "x"}, strEnc{}))
	require.NoError(t, stream.MsgRecv(new(strMsg), strEnc{}))

	cancel()

	done := make(chan struct{})
	go func() {
		_ = stream.MsgSend(&strMsg{S: "y"}, strEnc{})
		_ = stream.MsgRecv(new(strMsg), strEnc{})
		_ = stream.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("operations did not unblock after context cancel")
	}
}

// When the server goes away, the client sees a clean Unavailable error.
func TestConn_ServerCloseMidInvoke(t *testing.T) {
	addr, clientTLS, stop := startServer(t, echoHandler{}, Options{})

	ctx := context.Background()
	mt, err := Dial(ctx, addr, clientTLS, Options{})
	require.NoError(t, err)
	conn := drpcconn.NewFromMultiplexed(mt, drpcconn.Options{})
	defer func() { _ = conn.Close() }()

	stop() // server gone (drains + closes connections)

	out := new(strMsg)
	err = conn.Invoke(ctx, "/echo", strEnc{}, &strMsg{S: "hi"}, out)
	require.Error(t, err)
	require.Equal(t, codes.Unavailable, status.Code(err))
}

// A clean unary RPC followed by a clean connection close logs nothing on the
// server (clean-close suppression).
func TestConn_NoLogOnCleanClose(t *testing.T) {
	var mu sync.Mutex
	var logged []error
	opts := Options{Log: func(e error) {
		mu.Lock()
		logged = append(logged, e)
		mu.Unlock()
	}}

	addr, clientTLS, stop := startServer(t, echoHandler{}, opts)
	defer stop()

	ctx := context.Background()
	mt, err := Dial(ctx, addr, clientTLS, Options{})
	require.NoError(t, err)
	conn := drpcconn.NewFromMultiplexed(mt, drpcconn.Options{})

	out := new(strMsg)
	require.NoError(t, conn.Invoke(ctx, "/echo", strEnc{}, &strMsg{S: "hi"}, out))
	require.Equal(t, "echo:hi", out.S)
	require.NoError(t, conn.Close())

	stop() // fully drains the server before we inspect the log

	mu.Lock()
	defer mu.Unlock()
	require.Empty(t, logged, "clean RPC + close should not log: %v", logged)
}

// Many sequential streaming RPCs on one connection do not leak goroutines.
func TestConn_StreamingNoGoroutineLeak(t *testing.T) {
	addr, clientTLS, stop := startServer(t, echoHandler{}, Options{})
	defer stop()

	ctx := context.Background()
	mt, err := Dial(ctx, addr, clientTLS, Options{})
	require.NoError(t, err)
	conn := drpcconn.NewFromMultiplexed(mt, drpcconn.Options{})
	defer func() { _ = conn.Close() }()

	runtime.GC()
	base := runtime.NumGoroutine()

	for range 50 {
		stream, err := conn.NewStream(ctx, "/stream", strEnc{})
		require.NoError(t, err)
		require.NoError(t, stream.MsgSend(&strMsg{S: "x"}, strEnc{}))
		require.NoError(t, stream.MsgRecv(new(strMsg), strEnc{}))
		require.NoError(t, stream.CloseSend())
		require.NoError(t, stream.Close())
	}

	require.Eventually(t, func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= base+8
	}, 5*time.Second, 50*time.Millisecond, "goroutines did not settle (leak)")
}
