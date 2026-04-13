// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/zeebo/assert"
	"go.uber.org/goleak"

	"storj.io/drpc/drpctest"
)

// TestStress_SustainedConcurrentStreams opens 50 bidi streams on one
// connection, each exchanging 100 echo messages concurrently. This saturates
// the manageReader dispatch path (one reader fanning out to 50 packetQueues)
// and the MuxWriter batching path (50 goroutines calling WriteFrame). Each
// message encodes the stream's identity so we can detect cross-stream data
// corruption, which would indicate a routing bug in the manager or a buffer
// reuse bug in the packetQueue.
func TestStress_SustainedConcurrentStreams(t *testing.T) {
	defer goleak.VerifyNone(t)

	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	conn := createRawConnection(t, impl{
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
	}, ctx)
	defer func() { _ = conn.Close() }()
	cli := NewDRPCServiceClient(conn)

	const N = 50  // concurrent streams
	const M = 100 // messages per stream

	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		i := i
		ctx.Run(func(ctx context.Context) {
			select {
			case <-ctx.Done():
			case errs <- func() error {
				stream, err := cli.Method4(ctx)
				if err != nil {
					return fmt.Errorf("stream %d: open: %w", i, err)
				}
				for j := 0; j < M; j++ {
					val := int64(i*1000 + j) // encode stream identity
					if err := stream.Send(&In{In: val}); err != nil {
						return fmt.Errorf("stream %d: send %d: %w", i, j, err)
					}
					out, err := stream.Recv()
					if err != nil {
						return fmt.Errorf("stream %d: recv %d: %w", i, j, err)
					}
					if out.Out != val {
						return fmt.Errorf("stream %d: msg %d: got %d, want %d (cross-contamination?)", i, j, out.Out, val)
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

	for i := 0; i < N; i++ {
		assert.NoError(t, <-errs)
	}
}

// TestStress_RapidOpenCloseCycles opens and closes a bidi stream 500 times
// sequentially on one connection. Each cycle creates a stream, exchanges one
// message, then tears it down. This tests that stream ID allocation, the
// activeStreams map cleanup, and the invoke handshake (pdone channel) work
// correctly across many rapid create/destroy cycles without leaking resources.
func TestStress_RapidOpenCloseCycles(t *testing.T) {
	defer goleak.VerifyNone(t)

	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	conn := createRawConnection(t, impl{
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
	}, ctx)
	defer func() { _ = conn.Close() }()
	cli := NewDRPCServiceClient(conn)

	const K = 500

	for i := 0; i < K; i++ {
		stream, err := cli.Method4(ctx)
		assert.NoError(t, err)

		val := int64(i)
		assert.NoError(t, stream.Send(&In{In: val}))
		out, err := stream.Recv()
		assert.NoError(t, err)
		assert.Equal(t, out.Out, val)

		assert.NoError(t, stream.CloseSend())
		_, err = stream.Recv()
		assert.That(t, errors.Is(err, io.EOF))
	}
}

// TestStress_CancelStorm opens 30 streams that all exchange messages
// concurrently, then cancels ~50% of them (chosen randomly) with jitter so
// cancellations land at different points in the send/recv cycle. This races
// context cancellation against in-flight Send and Recv calls, exercising the
// stream's cancel propagation and cleanup. Surviving (non-cancelled) streams
// must complete without error, verifying cancel isolation.
func TestStress_CancelStorm(t *testing.T) {
	defer goleak.VerifyNone(t)

	seed := time.Now().UnixNano()
	t.Logf("random seed: %d", seed)

	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	conn := createRawConnection(t, impl{
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
	}, ctx)
	defer func() { _ = conn.Close() }()
	cli := NewDRPCServiceClient(conn)

	const N = 30
	const messagesPerStream = 20

	type entry struct {
		stream DRPCService_Method4Client
		cancel context.CancelFunc
	}

	entries := make([]entry, N)
	for i := 0; i < N; i++ {
		sctx, cancel := context.WithCancel(ctx)
		stream, err := cli.Method4(sctx)
		assert.NoError(t, err)
		entries[i] = entry{stream: stream, cancel: cancel}
	}

	// Decide which streams to cancel (~50%).
	rng := rand.New(rand.NewSource(seed))
	cancelled := make([]bool, N)
	for i := range entries {
		cancelled[i] = rng.Intn(2) == 0
	}

	// All streams exchange messages concurrently. Cancelled streams
	// will hit errors once the cancel goroutine fires their context;
	// surviving streams must complete without error.
	var wg sync.WaitGroup
	wg.Add(N)
	for i := range entries {
		i := i
		ctx.Run(func(_ context.Context) {
			defer wg.Done()
			defer entries[i].cancel()
			for j := 0; j < messagesPerStream; j++ {
				val := int64(i*1000 + j)
				if err := entries[i].stream.Send(&In{In: val}); err != nil {
					if !cancelled[i] {
						t.Errorf("surviving stream %d: send %d: %v", i, j, err)
					}
					return
				}
				out, err := entries[i].stream.Recv()
				if err != nil {
					if !cancelled[i] {
						t.Errorf("surviving stream %d: recv %d: %v", i, j, err)
					}
					return
				}
				if out.Out != val {
					t.Errorf("stream %d: msg %d: got %d, want %d", i, j, out.Out, val)
				}
			}
			if !cancelled[i] {
				_ = entries[i].stream.CloseSend()
			}
		})
	}

	// Fire cancels concurrently with traffic, with jitter.
	ctx.Run(func(_ context.Context) {
		time.Sleep(time.Millisecond) // let streams start sending
		for i := range entries {
			if cancelled[i] {
				time.Sleep(time.Duration(rng.Intn(500)) * time.Microsecond)
				entries[i].cancel()
			}
		}
	})

	wg.Wait()
}

// TestStress_ShutdownDuringActivity opens 20 streams all actively sending and
// receiving, then calls conn.Close(). This exercises the manager's terminate()
// path with traffic in flight: the MuxWriter must stop, manageReader must
// unblock, and all stream goroutines must exit. The test fails if conn.Close()
// doesn't return within 5 seconds (deadlock).
func TestStress_ShutdownDuringActivity(t *testing.T) {
	defer goleak.VerifyNone(t)

	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	started := make(chan struct{}, 20)
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

	const N = 20

	streams := make([]DRPCService_Method4Client, N)
	for i := 0; i < N; i++ {
		s, err := cli.Method4(ctx)
		assert.NoError(t, err)
		streams[i] = s
	}

	// Wait for all server handlers to start.
	for i := 0; i < N; i++ {
		<-started
	}

	// Start sending on all streams continuously.
	for i, s := range streams {
		i, s := i, s
		ctx.Run(func(_ context.Context) {
			for {
				if err := s.Send(&In{In: int64(i)}); err != nil {
					return
				}
				if _, err := s.Recv(); err != nil {
					return
				}
			}
		})
	}

	// Let traffic flow briefly.
	time.Sleep(10 * time.Millisecond)

	// Close the connection. Must return within timeout.
	done := make(chan error, 1)
	go func() { done <- conn.Close() }()

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		t.Fatal("conn.Close() deadlocked with active streams")
	}
}

// TestStress_MixedRPCTypes runs 30 goroutines each executing 10 rounds of a
// randomly chosen RPC type (unary, client-streaming, server-streaming, bidi)
// concurrently on one connection. Different RPC types have different frame
// sequences and stream lifecycles: unary is short-lived (invoke, response,
// close), client-streaming sends multiple frames before expecting a response,
// server-streaming reads until EOF, and bidi is long-lived bidirectional.
// Mixing them exercises the manager's ability to correctly interleave
// heterogeneous stream types on a shared transport.
func TestStress_MixedRPCTypes(t *testing.T) {
	defer goleak.VerifyNone(t)

	seed := time.Now().UnixNano()
	t.Logf("random seed: %d", seed)

	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	conn := createRawConnection(t, impl{
		// Method1: unary echo.
		Method1Fn: func(ctx context.Context, in *In) (*Out, error) {
			return &Out{Out: in.In}, nil
		},
		// Method2: client-streaming — sum inputs.
		Method2Fn: func(stream DRPCService_Method2Stream) error {
			var total int64
			for {
				in, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					return err
				}
				total += in.In
			}
			return stream.SendAndClose(&Out{Out: total})
		},
		// Method3: server-streaming — send N copies.
		Method3Fn: func(in *In, stream DRPCService_Method3Stream) error {
			for i := 0; i < int(in.In); i++ {
				if err := stream.Send(&Out{Out: in.In}); err != nil {
					return err
				}
			}
			return nil
		},
		// Method4: bidi echo.
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
	}, ctx)
	defer func() { _ = conn.Close() }()
	cli := NewDRPCServiceClient(conn)

	const N = 30
	const rounds = 10

	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		i := i
		ctx.Run(func(ctx context.Context) {
			select {
			case <-ctx.Done():
			case errs <- func() error {
				rng := rand.New(rand.NewSource(seed + int64(i)))
				for r := 0; r < rounds; r++ {
					switch rng.Intn(4) {
					case 0: // Unary
						out, err := cli.Method1(ctx, &In{In: int64(i*100 + r)})
						if err != nil {
							return fmt.Errorf("goroutine %d round %d: unary: %w", i, r, err)
						}
						if out.Out != int64(i*100+r) {
							return fmt.Errorf("goroutine %d round %d: unary: got %d want %d", i, r, out.Out, i*100+r)
						}

					case 1: // Client-streaming
						stream, err := cli.Method2(ctx)
						if err != nil {
							return fmt.Errorf("goroutine %d round %d: client-stream open: %w", i, r, err)
						}
						var want int64
						for k := 0; k < 5; k++ {
							v := int64(k + 1)
							want += v
							if err := stream.Send(&In{In: v}); err != nil {
								return fmt.Errorf("goroutine %d round %d: client-stream send: %w", i, r, err)
							}
						}
						out, err := stream.CloseAndRecv()
						if err != nil {
							return fmt.Errorf("goroutine %d round %d: client-stream close: %w", i, r, err)
						}
						if out.Out != want {
							return fmt.Errorf("goroutine %d round %d: client-stream: got %d want %d", i, r, out.Out, want)
						}

					case 2: // Server-streaming
						count := int64(3)
						stream, err := cli.Method3(ctx, &In{In: count})
						if err != nil {
							return fmt.Errorf("goroutine %d round %d: server-stream open: %w", i, r, err)
						}
						var got int
						for {
							out, err := stream.Recv()
							if errors.Is(err, io.EOF) {
								break
							}
							if err != nil {
								return fmt.Errorf("goroutine %d round %d: server-stream recv: %w", i, r, err)
							}
							if out.Out != count {
								return fmt.Errorf("goroutine %d round %d: server-stream: got %d want %d", i, r, out.Out, count)
							}
							got++
						}
						if got != int(count) {
							return fmt.Errorf("goroutine %d round %d: server-stream: got %d msgs want %d", i, r, got, count)
						}

					case 3: // Bidi
						stream, err := cli.Method4(ctx)
						if err != nil {
							return fmt.Errorf("goroutine %d round %d: bidi open: %w", i, r, err)
						}
						for k := 0; k < 5; k++ {
							val := int64(i*10000 + r*100 + k)
							if err := stream.Send(&In{In: val}); err != nil {
								return fmt.Errorf("goroutine %d round %d: bidi send: %w", i, r, err)
							}
							out, err := stream.Recv()
							if err != nil {
								return fmt.Errorf("goroutine %d round %d: bidi recv: %w", i, r, err)
							}
							if out.Out != val {
								return fmt.Errorf("goroutine %d round %d: bidi: got %d want %d", i, r, out.Out, val)
							}
						}
						if err := stream.CloseSend(); err != nil {
							return fmt.Errorf("goroutine %d round %d: bidi close: %w", i, r, err)
						}
						_, err = stream.Recv()
						if !errors.Is(err, io.EOF) {
							return fmt.Errorf("goroutine %d round %d: bidi final: got %v want EOF", i, r, err)
						}
					}
				}
				return nil
			}():
			}
		})
	}

	for i := 0; i < N; i++ {
		assert.NoError(t, <-errs)
	}
}

// TestStress_ConcurrentCancelCloseTransportClose fires context cancellation,
// conn.Close(), and transport.Close() nearly simultaneously while 10 streams
// are actively exchanging messages. This is the worst-case shutdown scenario:
// three independent shutdown paths (cancel propagation, manager.Close,
// transport EOF) all racing to call terminate(). The manager's terminate() is
// idempotent via sigs.term first-wins, so only one should win; the rest must
// be no-ops. Deadlock within 5 seconds is a failure.
func TestStress_ConcurrentCancelCloseTransportClose(t *testing.T) {
	defer goleak.VerifyNone(t)

	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	started := make(chan struct{}, 10)
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

	const N = 10

	cancels := make([]context.CancelFunc, N)
	streams := make([]DRPCService_Method4Client, N)
	for i := 0; i < N; i++ {
		sctx, cancel := context.WithCancel(ctx)
		s, err := cli.Method4(sctx)
		assert.NoError(t, err)
		cancels[i] = cancel
		streams[i] = s
	}

	for i := 0; i < N; i++ {
		<-started
	}

	// Start sending on all streams.
	for i, s := range streams {
		i, s := i, s
		ctx.Run(func(_ context.Context) {
			for {
				if err := s.Send(&In{In: int64(i)}); err != nil {
					return
				}
				if _, err := s.Recv(); err != nil {
					return
				}
			}
		})
	}

	time.Sleep(10 * time.Millisecond)

	// Fire all three shutdown mechanisms nearly simultaneously.
	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		for _, c := range cancels {
			c()
		}
	}()

	go func() {
		defer wg.Done()
		_ = conn.Close()
	}()

	go func() {
		defer wg.Done()
		_ = conn.Transport().Close()
	}()

	// Must complete within timeout — no deadlock.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		t.Fatal("triple-race shutdown deadlocked")
	}
}

// TestStress_ConcurrentUnary runs 100 goroutines each making 50 unary RPCs
// on one connection. Each unary call creates a stream, does the invoke
// handshake, sends request, receives response, and tears down, so this
// produces 5000 rapid stream lifecycles. Complements TestStress_BurstUnary
// below, which tests instantaneous burst contention rather than sustained
// throughput.
func TestStress_ConcurrentUnary(t *testing.T) {
	defer goleak.VerifyNone(t)

	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	conn := createRawConnection(t, impl{
		Method1Fn: func(ctx context.Context, in *In) (*Out, error) {
			return &Out{Out: in.In}, nil
		},
	}, ctx)
	defer func() { _ = conn.Close() }()
	cli := NewDRPCServiceClient(conn)

	const N = 100
	const M = 50

	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		i := i
		ctx.Run(func(ctx context.Context) {
			select {
			case <-ctx.Done():
			case errs <- func() error {
				for j := 0; j < M; j++ {
					val := int64(i*1000 + j)
					out, err := cli.Method1(ctx, &In{In: val})
					if err != nil {
						return fmt.Errorf("goroutine %d call %d: %w", i, j, err)
					}
					if out.Out != val {
						return fmt.Errorf("goroutine %d call %d: got %d want %d", i, j, out.Out, val)
					}
				}
				return nil
			}():
			}
		})
	}

	for i := 0; i < N; i++ {
		assert.NoError(t, <-errs)
	}
}

// TestStress_BurstUnary fires 1000 goroutines each making a single unary RPC
// simultaneously. All 1000 invokes hit the pdone channel at once, testing
// burst contention on the invoke handshake and the MuxWriter's ability to
// batch a thundering herd of WriteFrame calls.
func TestStress_BurstUnary(t *testing.T) {
	defer goleak.VerifyNone(t)

	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	conn := createRawConnection(t, impl{
		Method1Fn: func(ctx context.Context, in *In) (*Out, error) {
			return &Out{Out: in.In}, nil
		},
	}, ctx)
	defer func() { _ = conn.Close() }()
	cli := NewDRPCServiceClient(conn)

	const N = 1000

	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		i := i
		ctx.Run(func(ctx context.Context) {
			select {
			case <-ctx.Done():
			case errs <- func() error {
				val := int64(i)
				out, err := cli.Method1(ctx, &In{In: val})
				if err != nil {
					return fmt.Errorf("goroutine %d: %w", i, err)
				}
				if out.Out != val {
					return fmt.Errorf("goroutine %d: got %d want %d", i, out.Out, val)
				}
				return nil
			}():
			}
		})
	}

	for i := 0; i < N; i++ {
		assert.NoError(t, <-errs)
	}
}
