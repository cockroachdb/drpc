// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

//go:build !gogo && !custom
// +build !gogo,!custom

package integration

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"

	"github.com/zeebo/assert"

	"storj.io/drpc"
	"storj.io/drpc/drpcconn"
	"storj.io/drpc/drpcmetadata"
	"storj.io/drpc/drpcmux"
	"storj.io/drpc/drpcserver"
	"storj.io/drpc/drpctest"
	"storj.io/drpc/drpcwire"
)

// TestCompression_Unary verifies a simple unary RPC round-trips correctly
// when Snappy compression is enabled on the connection.
func TestCompression_Unary(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cli, close := createConnection(t, standardImpl, drpc.CompressionSnappy)
	defer close()

	out, err := cli.Method1(ctx, &In{In: 1})
	assert.NoError(t, err)
	assert.True(t, Equal(out, &Out{Out: 1}))
}

// TestCompression_UnaryWithData sends a 1 KiB payload through a compressed
// unary RPC and checks the data survives the compress/decompress round-trip.
func TestCompression_UnaryWithData(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cli, close := createConnection(t, standardImpl, drpc.CompressionSnappy)
	defer close()

	payload := data(1024)
	out, err := cli.Method1(ctx, &In{In: 1, Data: payload})
	assert.NoError(t, err)
	assert.DeepEqual(t, out.Data, payload)
}

// TestCompression_ClientStream exercises a client-streaming RPC with Snappy,
// sending two messages before closing and receiving the aggregated response.
func TestCompression_ClientStream(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cli, close := createConnection(t, standardImpl, drpc.CompressionSnappy)
	defer close()

	stream, err := cli.Method2(ctx)
	assert.NoError(t, err)
	assert.NoError(t, stream.Send(&In{In: 2}))
	assert.NoError(t, stream.Send(&In{In: 2}))
	out, err := stream.CloseAndRecv()
	assert.NoError(t, err)
	assert.True(t, Equal(out, &Out{Out: 2}))
}

// TestCompression_ServerStream exercises a server-streaming RPC with Snappy,
// reading all streamed messages and verifying the count matches expectations.
func TestCompression_ServerStream(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cli, close := createConnection(t, standardImpl, drpc.CompressionSnappy)
	defer close()

	stream, err := cli.Method3(ctx, &In{In: 3})
	assert.NoError(t, err)
	count := 0
	for {
		out, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		assert.NoError(t, err)
		assert.True(t, Equal(out, &Out{Out: 3}))
		count++
	}
	assert.Equal(t, count, 3)
}

// TestCompression_BidiStream exercises a bidirectional streaming RPC with
// Snappy using an echo server, verifying each send/recv pair round-trips.
func TestCompression_BidiStream(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	echoServer := impl{
		Method1Fn: standardImpl.Method1Fn,
		Method2Fn: standardImpl.Method2Fn,
		Method3Fn: standardImpl.Method3Fn,
		Method4Fn: func(stream DRPCService_Method4Stream) error {
			for {
				msg, err := stream.Recv()
				if err != nil {
					return nil
				}
				if err := stream.Send(&Out{Out: msg.In}); err != nil {
					return err
				}
			}
		},
	}

	cli, close := createConnection(t, echoServer, drpc.CompressionSnappy)
	defer close()

	stream, err := cli.Method4(ctx)
	assert.NoError(t, err)
	for i := int64(0); i < 10; i++ {
		assert.NoError(t, stream.Send(&In{In: i}))
		out, err := stream.Recv()
		assert.NoError(t, err)
		assert.Equal(t, out.Out, i)
	}
	assert.NoError(t, stream.CloseSend())
	_, err = stream.Recv()
	assert.That(t, errors.Is(err, io.EOF))
}

// TestCompression_NoCompressionBackwardCompat confirms that a client without
// compression configured still works against the same server infrastructure.
func TestCompression_NoCompressionBackwardCompat(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	// Uncompressed client talks to server — should work as before.
	cli, close := createConnection(t, standardImpl)
	defer close()

	out, err := cli.Method1(ctx, &In{In: 1})
	assert.NoError(t, err)
	assert.True(t, Equal(out, &Out{Out: 1}))
}

// TestCompression_UnsupportedCompression verifies that a caller manually
// injecting an unknown compression metadata key has it stripped, and the
// RPC succeeds as a normal uncompressed call.
func TestCompression_UnsupportedCompression(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	// A caller injecting the reserved compression metadata key without
	// configuring WithCompression should have the key stripped — the RPC
	// succeeds as a normal uncompressed call.
	c1, c2 := net.Pipe()
	mux := drpcmux.New()
	assert.NoError(t, DRPCRegisterService(mux, standardImpl))
	srv := drpcserver.New(mux)
	ctx.Run(func(ctx context.Context) { _ = srv.ServeOne(ctx, c1) })
	conn := drpcconn.NewWithOptions(c2, drpcconn.Options{})
	defer func() { _ = conn.Close() }()
	cli := NewDRPCServiceClient(conn)

	rpcCtx := drpcmetadata.AppendToOutgoingContext(ctx, map[string]string{
		drpcwire.CompressionMetadataKey: "unknown-algo",
	})
	_, err := cli.Method1(rpcCtx, &In{In: 1})
	assert.NoError(t, err)
}
