// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package integration

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/zeebo/assert"
	"storj.io/drpc/drpcconn"
	"storj.io/drpc/drpcmanager"
	"storj.io/drpc/drpcmetrics"
	"storj.io/drpc/drpcmux"
	"storj.io/drpc/drpcserver"
	"storj.io/drpc/drpctest"
)

//
// test metric implementations
//

type testCounter struct {
	mu     sync.Mutex
	total_ float64
	count_ int
}

func (c *testCounter) Inc(v int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total_ += float64(v)
	c.count_++
}

func (c *testCounter) total() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total_
}

func (c *testCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count_
}

//
// connection helpers
//

func createMeteredClientConnection(
	t testing.TB, server DRPCServiceServer, metrics drpcmetrics.ClientMetrics,
) (DRPCServiceClient, func()) {
	ctx := drpctest.NewTracker(t)
	c1, c2 := net.Pipe()
	mux := drpcmux.New()
	assert.NoError(t, DRPCRegisterService(mux, server))
	srv := drpcserver.New(mux)
	ctx.Run(func(ctx context.Context) { _ = srv.ServeOne(ctx, c1) })
	conn := drpcconn.NewWithOptions(c2, drpcconn.Options{
		Manager:        drpcmanager.Options{},
		Metrics:        metrics,
		CollectMetrics: true,
	})
	return NewDRPCServiceClient(conn), func() {
		_ = conn.Close()
		ctx.Close()
	}
}

//
// tests
//

func TestClientByteMetrics(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	sent := &testCounter{}
	recv := &testCounter{}
	cli, close := createMeteredClientConnection(t, standardImpl, drpcmetrics.ClientMetrics{
		BytesSent: sent,
		BytesRecv: recv,
	})
	defer close()

	// Unary RPC.
	out, err := cli.Method1(ctx, in(1))
	assert.NoError(t, err)
	assert.True(t, Equal(out, &Out{Out: 1}))

	sentAfterUnary := sent.total()
	recvAfterUnary := recv.total()
	assert.That(t, sentAfterUnary > 0)
	assert.That(t, recvAfterUnary > 0)

	// Server-streaming RPC: should increase counters further.
	stream, err := cli.Method3(ctx, in(3))
	assert.NoError(t, err)
	for {
		_, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		assert.NoError(t, err)
	}
	assert.NoError(t, stream.Close())

	assert.That(t, sent.total() > sentAfterUnary)
	assert.That(t, recv.total() > recvAfterUnary)
}

func TestClientByteMetricsPartialNil(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	sent := &testCounter{}
	cli, close := createMeteredClientConnection(t, standardImpl, drpcmetrics.ClientMetrics{
		BytesSent: sent,
		// BytesRecv intentionally nil.
	})
	defer close()

	out, err := cli.Method1(ctx, in(1))
	assert.NoError(t, err)
	assert.True(t, Equal(out, &Out{Out: 1}))
	assert.That(t, sent.total() > 0)
}

func TestClientByteMetricsNotCollected(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	sent := &testCounter{}
	recv := &testCounter{}

	c1, c2 := net.Pipe()
	mux := drpcmux.New()
	assert.NoError(t, DRPCRegisterService(mux, standardImpl))
	srv := drpcserver.New(mux)
	ctx.Run(func(ctx2 context.Context) { _ = srv.ServeOne(ctx2, c1) })
	conn := drpcconn.NewWithOptions(c2, drpcconn.Options{
		Metrics: drpcmetrics.ClientMetrics{
			BytesSent: sent,
			BytesRecv: recv,
		},
	})
	cli := NewDRPCServiceClient(conn)

	out, err := cli.Method1(ctx, in(1))
	assert.NoError(t, err)
	assert.True(t, Equal(out, &Out{Out: 1}))

	// CollectMetrics is false, so no metrics should be collected.
	assert.Equal(t, sent.total(), 0.0)
	assert.Equal(t, recv.total(), 0.0)

	_ = conn.Close()
}

func TestServerTLSHandshakeErrorMetric(t *testing.T) {
	tlsErrors := &testCounter{}

	mux := drpcmux.New()
	assert.NoError(t, DRPCRegisterService(mux, standardImpl))
	srv := drpcserver.NewWithOptions(mux, drpcserver.Options{
		Metrics: drpcserver.ServerMetrics{
			TLSHandshakeErrors: tlsErrors,
		},
	})

	// Create a TLS-wrapped connection with an invalid handshake:
	// the server expects a TLS client hello but receives plain text.
	c1, c2 := net.Pipe()

	tlsServerConn := tls.Server(c1, &tls.Config{
		// Intentionally no certificates — handshake will fail.
	})

	done := make(chan error, 1)
	go func() {
		done <- srv.ServeOne(context.Background(), tlsServerConn)
	}()

	// Write garbage to trigger a TLS handshake failure on the server.
	_, _ = c2.Write([]byte("not a tls handshake"))

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for server to exit")
	}

	_ = c2.Close()
	assert.Equal(t, tlsErrors.count(), 1)
}
