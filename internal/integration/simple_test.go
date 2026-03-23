// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/zeebo/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"storj.io/drpc/drpcconn"
	"storj.io/drpc/drpcmux"
	"storj.io/drpc/drpcserver"
	"storj.io/drpc/drpcstats"
	"storj.io/drpc/drpctest"
)

func TestSimple(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cli, close := createConnection(t, standardImpl)
	defer close()

	{
		out, err := cli.Method1(ctx, &In{In: 1})
		assert.NoError(t, err)
		assert.True(t, Equal(out, &Out{Out: 1}))
	}

	{
		stream, err := cli.Method2(ctx)
		assert.NoError(t, err)
		assert.NoError(t, stream.Send(&In{In: 2}))
		assert.NoError(t, stream.Send(&In{In: 2}))
		out, err := stream.CloseAndRecv()
		assert.NoError(t, err)
		assert.True(t, Equal(out, &Out{Out: 2}))
	}

	{
		stream, err := cli.Method3(ctx, &In{In: 3})
		assert.NoError(t, err)
		for {
			out, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			assert.NoError(t, err)
			assert.True(t, Equal(out, &Out{Out: 3}))
		}
	}

	{
		stream, err := cli.Method4(ctx)
		assert.NoError(t, err)
		assert.NoError(t, stream.Send(&In{In: 4}))
		assert.NoError(t, stream.Send(&In{In: 4}))
		assert.NoError(t, stream.Send(&In{In: 4}))
		assert.NoError(t, stream.Send(&In{In: 4}))
		assert.NoError(t, stream.CloseSend())
		for {
			out, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			assert.NoError(t, err)
			assert.True(t, Equal(out, &Out{Out: 4}))
		}
	}

	{
		_, err := cli.Method1(ctx, &In{In: 5})
		assert.Error(t, err)
		st, ok := status.FromError(err)
		assert.That(t, ok)
		assert.Equal(t, st.Code(), codes.Code(5))
	}
}

func TestMultiplexedStreams(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	// Echo server: sends back each received message immediately.
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

	cli, close := createConnection(t, echoServer)
	defer close()

	// Open two bidi streams on the same connection.
	s1, err := cli.Method4(ctx)
	assert.NoError(t, err)

	s2, err := cli.Method4(ctx)
	assert.NoError(t, err)

	// Send on both streams interleaved.
	assert.NoError(t, s1.Send(&In{In: 1}))
	assert.NoError(t, s2.Send(&In{In: 2}))

	// Receive from both: each stream gets its own response.
	out1, err := s1.Recv()
	assert.NoError(t, err)
	assert.Equal(t, out1.Out, int64(1))

	out2, err := s2.Recv()
	assert.NoError(t, err)
	assert.Equal(t, out2.Out, int64(2))

	// Close both streams.
	assert.NoError(t, s1.CloseSend())
	assert.NoError(t, s2.CloseSend())

	_, err = s1.Recv()
	assert.That(t, errors.Is(err, io.EOF))

	_, err = s2.Recv()
	assert.That(t, errors.Is(err, io.EOF))
}

func TestConcurrentStreams(t *testing.T) {
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

	cli, close := createConnection(t, echoServer)
	defer close()

	const numStreams = 10
	const numMessages = 20

	errs := make(chan error, numStreams)
	for i := 0; i < numStreams; i++ {
		i := i
		ctx.Run(func(ctx context.Context) {
			select {
			case <-ctx.Done():
			case errs <- func() error {
				stream, err := cli.Method4(ctx)
				if err != nil {
					return fmt.Errorf("stream %d: open: %w", i, err)
				}
				for j := 0; j < numMessages; j++ {
					val := int64(i*1000 + j)
					if err := stream.Send(&In{In: val}); err != nil {
						return fmt.Errorf("stream %d: send %d: %w", i, j, err)
					}
					out, err := stream.Recv()
					if err != nil {
						return fmt.Errorf("stream %d: recv %d: %w", i, j, err)
					}
					if out.Out != val {
						return fmt.Errorf("stream %d: msg %d: got %d, want %d", i, j, out.Out, val)
					}
				}
				if err := stream.CloseSend(); err != nil {
					return fmt.Errorf("stream %d: close send: %w", i, err)
				}
				_, err = stream.Recv()
				if !errors.Is(err, io.EOF) {
					return fmt.Errorf("stream %d: final recv: got %v, want EOF", i, err)
				}
				return nil
			}():
			}
		})
	}

	for i := 0; i < numStreams; i++ {
		assert.NoError(t, <-errs)
	}
}

func TestConcurrent(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cli, close := createConnection(t, standardImpl)
	defer close()

	const N = 1000
	errs := make(chan error)
	for i := 0; i < N; i++ {
		ctx.Run(func(ctx context.Context) {
			select {
			case <-ctx.Done():
			case errs <- func() error {
				out, err := cli.Method1(ctx, &In{In: 1})
				if err != nil {
					return err
				} else if out.Out != 1 {
					return fmt.Errorf("wrong result %d", out.Out)
				} else {
					return nil
				}
			}():
			}
		})
	}
	for i := 0; i < N; i++ {
		assert.NoError(t, <-errs)
	}
}

func TestServerStats(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	c1, c2 := net.Pipe()
	mux := drpcmux.New()
	_ = DRPCRegisterService(mux, standardImpl)

	srv := drpcserver.NewWithOptions(mux, drpcserver.Options{
		CollectStats: true,
	})
	ctx.Run(func(ctx context.Context) { _ = srv.ServeOne(ctx, c1) })

	conn := drpcconn.NewWithOptions(c2, drpcconn.Options{})
	defer func() { _ = conn.Close() }()
	cli := NewDRPCServiceClient(conn)

	assert.Equal(t, srv.Stats(), map[string]drpcstats.Stats{})

	_, err := cli.Method1(ctx, in(5))
	assert.Error(t, err)

	assert.Equal(t, srv.Stats(), map[string]drpcstats.Stats{
		"/service.Service/Method1": {Read: 2, Written: 9},
	})

	_, err = cli.Method1(ctx, in(1))
	assert.NoError(t, err)

	assert.Equal(t, srv.Stats(), map[string]drpcstats.Stats{
		"/service.Service/Method1": {Read: 2 + 2, Written: 9 + 2},
	})

	stream, err := cli.Method3(ctx, in(3))
	assert.NoError(t, err)
	for i := 0; i < 3; i++ {
		_, err := stream.Recv()
		assert.NoError(t, err)
	}
	_, err = stream.Recv()
	assert.That(t, errors.Is(err, io.EOF))
	assert.NoError(t, stream.Close())

	assert.Equal(t, srv.Stats(), map[string]drpcstats.Stats{
		"/service.Service/Method1": {Read: 2 + 2, Written: 9 + 2},
		"/service.Service/Method3": {Read: 2, Written: 6},
	})
}
