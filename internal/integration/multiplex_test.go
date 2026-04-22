// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package integration

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeebo/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"storj.io/drpc/drpcpool"
	"storj.io/drpc/drpctest"
)

// TestMultiplex_CancelIsolation verifies that canceling one stream's context
// does not affect other concurrent streams on the same connection.
func TestMultiplex_CancelIsolation(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	started := make(chan struct{}, 3)
	cli, close := createConnection(t, impl{
		Method4Fn: func(stream DRPCService_Method4Stream) error {
			started <- struct{}{}
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
	})
	defer close()

	// Open 3 bidi streams with independent contexts.
	ctx1, cancel1 := context.WithCancel(ctx)
	defer cancel1()
	s1, err := cli.Method4(ctx1)
	assert.NoError(t, err)

	ctx2, cancel2 := context.WithCancel(ctx)
	defer cancel2()
	s2, err := cli.Method4(ctx2)
	assert.NoError(t, err)

	ctx3, cancel3 := context.WithCancel(ctx)
	defer cancel3()
	s3, err := cli.Method4(ctx3)
	assert.NoError(t, err)

	// Wait for all server handlers to start.
	<-started
	<-started
	<-started

	// Cancel stream 2.
	cancel2()

	// Verify stream 2 is dead. This blocks until the cancel propagates.
	_, err = s2.Recv()
	assert.Error(t, err)
	st, ok := status.FromError(err)
	assert.That(t, ok)
	assert.Equal(t, st.Code(), codes.Canceled)

	// Streams 1 and 3 should still work.
	assert.NoError(t, s1.Send(in(10)))
	out, err := s1.Recv()
	assert.NoError(t, err)
	assert.Equal(t, out.Out, int64(10))

	assert.NoError(t, s3.Send(in(30)))
	out, err = s3.Recv()
	assert.NoError(t, err)
	assert.Equal(t, out.Out, int64(30))

	// Clean up remaining streams.
	assert.NoError(t, s1.CloseSend())
	assert.NoError(t, s3.CloseSend())
}

// TestMultiplex_ErrorIsolation verifies that a server handler returning an
// error on one stream does not affect other concurrent streams.
func TestMultiplex_ErrorIsolation(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	// Handler reads first message: In == -1 triggers an error, otherwise echo.
	cli, close := createConnection(t, impl{
		Method4Fn: func(stream DRPCService_Method4Stream) error {
			msg, err := stream.Recv()
			if err != nil {
				return nil
			}
			if msg.In == -1 {
				return status.Error(codes.InvalidArgument, "bad input")
			}
			if err := stream.Send(&Out{Out: msg.In}); err != nil {
				return err
			}
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
	})
	defer close()

	s1, err := cli.Method4(ctx)
	assert.NoError(t, err)

	s2, err := cli.Method4(ctx)
	assert.NoError(t, err)

	// Trigger error on stream 1.
	assert.NoError(t, s1.Send(in(-1)))

	// Send normal message on stream 2.
	assert.NoError(t, s2.Send(in(42)))

	// Stream 1 should receive the server error.
	_, err = s1.Recv()
	assert.Error(t, err)
	st, ok := status.FromError(err)
	assert.That(t, ok)
	assert.Equal(t, st.Code(), codes.InvalidArgument)

	// Stream 2 should be unaffected.
	out, err := s2.Recv()
	assert.NoError(t, err)
	assert.Equal(t, out.Out, int64(42))

	// Stream 2 keeps working after stream 1 is dead.
	assert.NoError(t, s2.Send(in(100)))
	out, err = s2.Recv()
	assert.NoError(t, err)
	assert.Equal(t, out.Out, int64(100))

	// Clean up.
	assert.NoError(t, s2.CloseSend())
	_, err = s2.Recv()
	assert.That(t, errors.Is(err, io.EOF))
}

// TestMultiplex_ConnCloseWithActiveStreams verifies that closing a connection
// with multiple active streams terminates all of them and does not deadlock.
func TestMultiplex_ConnCloseWithActiveStreams(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	started := make(chan struct{}, 3)
	conn := createRawConnection(t, impl{
		Method4Fn: func(stream DRPCService_Method4Stream) error {
			started <- struct{}{}
			<-stream.Context().Done()
			return stream.Context().Err()
		},
	}, ctx)
	cli := NewDRPCServiceClient(conn)

	// Open 3 streams whose handlers block until canceled.
	const N = 3
	streams := make([]DRPCService_Method4Client, N)
	for i := 0; i < N; i++ {
		s, err := cli.Method4(ctx)
		assert.NoError(t, err)
		streams[i] = s
	}

	// Wait for all handlers to be running.
	for i := 0; i < N; i++ {
		<-started
	}

	// conn.Close triggers manager.Close which must not deadlock.
	done := make(chan error, 1)
	go func() { done <- conn.Close() }()

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		t.Fatal("conn.Close() deadlocked with active streams")
	}

	// All streams should be terminated.
	for i, s := range streams {
		_, err := s.Recv()
		assert.Error(t, err)
		t.Logf("stream %d: %v", i, err)
	}
}

// TestMultiplex_TransportCloseTerminatesAllStreams verifies that an external
// transport failure terminates all active streams on the connection.
func TestMultiplex_TransportCloseTerminatesAllStreams(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	started := make(chan struct{}, 3)
	conn := createRawConnection(t, impl{
		Method4Fn: func(stream DRPCService_Method4Stream) error {
			started <- struct{}{}
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
	}, ctx)
	cli := NewDRPCServiceClient(conn)

	// Open 3 streams.
	const N = 3
	streams := make([]DRPCService_Method4Client, N)
	for i := 0; i < N; i++ {
		s, err := cli.Method4(ctx)
		assert.NoError(t, err)
		streams[i] = s
	}

	// Wait for all handlers.
	for i := 0; i < N; i++ {
		<-started
	}

	// Verify streams work before the failure.
	assert.NoError(t, streams[0].Send(in(1)))
	out, err := streams[0].Recv()
	assert.NoError(t, err)
	assert.Equal(t, out.Out, int64(1))

	// Simulate transport failure.
	assert.NoError(t, conn.Transport().Close())

	// All streams should receive errors.
	for i, s := range streams {
		_, err := s.Recv()
		assert.Error(t, err)
		t.Logf("stream %d: %v", i, err)
	}

	// Connection should close cleanly after transport failure.
	_ = conn.Close()
}

func TestMultiplex_PoolMaxStreamsPerConn(t *testing.T) {
	tctx := drpctest.NewTracker(t)
	defer tctx.Close()

	const (
		totalStreams      = 100
		streamsPerConn    = 10
		expectedConns     = totalStreams / streamsPerConn
	)

	started := make(chan struct{}, totalStreams)
	server := impl{
		Method4Fn: func(stream DRPCService_Method4Stream) error {
			started <- struct{}{}
			<-stream.Context().Done()
			return nil
		},
	}

	var dials atomic.Int32
	pool := drpcpool.New[string](drpcpool.Options{
		MaxStreamsPerConn: streamsPerConn,
	})
	defer func() { _ = pool.Close() }()

	conn := pool.Get(tctx, "key", func(ctx context.Context, key string) (drpcpool.Conn, error) {
		dials.Add(1)
		return createRawConnection(t, server, tctx), nil
	})
	cli := NewDRPCServiceClient(conn)

	streams := make([]DRPCService_Method4Client, totalStreams)
	for i := range streams {
		s, err := cli.Method4(tctx)
		assert.NoError(t, err)
		streams[i] = s
	}

	for i := 0; i < totalStreams; i++ {
		<-started
	}

	assert.Equal(t, int(dials.Load()), expectedConns)

	for _, s := range streams {
		assert.NoError(t, s.CloseSend())
	}
}
