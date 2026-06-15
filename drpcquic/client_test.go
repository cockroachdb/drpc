// Copyright (C) 2026 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcquic

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"storj.io/drpc"
	"storj.io/drpc/drpcconn"
)

// Unary smoke test over the full Dial/Serve/Invoke path.
func TestConn_Unary(t *testing.T) {
	addr, clientTLS, stop := startServer(t, echoHandler{}, Options{})
	defer stop()

	ctx := context.Background()
	mt, err := Dial(ctx, addr, clientTLS, Options{})
	require.NoError(t, err)
	conn := drpcconn.NewFromMultiplexed(mt, drpcconn.Options{})
	defer func() { _ = conn.Close() }()

	out := new(strMsg)
	require.NoError(t, conn.Invoke(ctx, "/echo", strEnc{}, &strMsg{S: "hi"}, out))
	require.Equal(t, "echo:hi", out.S)
}

// barrierHandler blocks each RPC until `arrived` reaches zero, i.e. until all
// expected RPCs are in flight at once. If unary Invokes were serialized, only
// one would ever arrive and the barrier would never release.
type barrierHandler struct{ arrived sync.WaitGroup }

func (b *barrierHandler) HandleRPC(stream drpc.Stream, rpc string) error {
	in := new(strMsg)
	if err := stream.MsgRecv(in, strEnc{}); err != nil {
		return err
	}
	b.arrived.Done()
	b.arrived.Wait() // block until all expected RPCs have arrived concurrently
	return stream.MsgSend(&strMsg{S: "echo:" + in.S}, strEnc{})
}

// Concurrent unary Invokes on one QUIC conn must all reach the server before any
// returns — proving they are not serialized on c.mu (the Invoke concurrency fix).
func TestConn_ConcurrentInvoke(t *testing.T) {
	const n = 4
	h := &barrierHandler{}
	h.arrived.Add(n)

	addr, clientTLS, stop := startServer(t, h, Options{})
	defer stop()

	ctx := context.Background()
	mt, err := Dial(ctx, addr, clientTLS, Options{})
	require.NoError(t, err)
	conn := drpcconn.NewFromMultiplexed(mt, drpcconn.Options{})
	defer func() { _ = conn.Close() }()

	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			out := new(strMsg)
			want := fmt.Sprintf("m%d", i)
			e := conn.Invoke(ctx, "/echo", strEnc{}, &strMsg{S: want}, out)
			if e == nil && out.S != "echo:"+want {
				e = fmt.Errorf("bad echo: got %q want %q", out.S, "echo:"+want)
			}
			errCh <- e
		}(i)
	}

	deadline := time.After(10 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case e := <-errCh:
			require.NoError(t, e)
		case <-deadline:
			t.Fatal("concurrent Invokes did not all complete — likely serialized on c.mu")
		}
	}
}
