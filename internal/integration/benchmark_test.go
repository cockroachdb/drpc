// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zeebo/assert"
	"google.golang.org/protobuf/proto"

	"storj.io/drpc/drpcconn"
	"storj.io/drpc/drpctest"
)

//
// benchmark infrastructure
//

var benchTransports = []struct {
	name   string
	create func(testing.TB, DRPCServiceServer, *drpctest.Tracker) *drpcconn.Conn
}{
	{"Pipe", createPipeConnection},
	{"TCP", createTCPConnection},
}

var benchPayloads = []struct {
	name string
	data []byte
}{
	{"Small", nil},
	{"1KB", make([]byte, 1<<10)},
	{"64KB", make([]byte, 64<<10)},
	{"1MB", make([]byte, 1<<20)},
}

// benchEchoServer echoes back unary requests (Method1) and bidi messages
// (Method4), and drains client-streaming requests (Method2).
var benchEchoServer = impl{
	Method1Fn: func(ctx context.Context, in *In) (*Out, error) {
		return &Out{Out: in.In, Data: in.Data}, nil
	},
	Method2Fn: func(stream DRPCService_Method2Stream) error {
		for {
			_, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
		}
		return stream.SendAndClose(&Out{Out: 0})
	},
	Method3Fn: func(in *In, stream DRPCService_Method3Stream) error {
		out := &Out{Out: 1, Data: in.Data}
		for i := int64(0); i < in.In; i++ {
			if err := stream.Send(out); err != nil {
				return err
			}
		}
		return nil
	},
	Method4Fn: func(stream DRPCService_Method4Stream) error {
		for {
			msg, err := stream.Recv()
			if err != nil {
				return nil
			}
			if err := stream.Send(&Out{Out: msg.In, Data: msg.Data}); err != nil {
				return err
			}
		}
	},
}

// BenchmarkUnaryRPC measures the full round-trip cost of a unary RPC: stream
// creation, invoke handshake, request send, response receive, and stream
// teardown. Each b.N iteration is one complete RPC lifecycle.
func BenchmarkUnaryRPC(b *testing.B) {
	for _, tr := range benchTransports {
		b.Run(tr.name, func(b *testing.B) {
			for _, pl := range benchPayloads {
				b.Run(pl.name, func(b *testing.B) {
					ctx := drpctest.NewTracker(b)
					conn := tr.create(b, benchEchoServer, ctx)
					defer func() {
						_ = conn.Close()
						ctx.Close()
					}()
					cli := NewDRPCServiceClient(conn)

					in := &In{In: 1, Data: pl.data}
					b.SetBytes(int64(proto.Size(in)))
					b.ReportAllocs()
					b.ResetTimer()

					for i := 0; i < b.N; i++ {
						out, err := cli.Method1(ctx, in)
						if err != nil {
							b.Fatal(err)
						}
						if out.Out != 1 {
							b.Fatalf("got %d, want 1", out.Out)
						}
					}
				})
			}
		})
	}
}

// BenchmarkStreamRoundTrip measures the per-message echo latency on an
// established bidi stream. Stream creation is excluded from the timer.
// Comparing this with BenchmarkUnaryRPC reveals the stream creation overhead.
func BenchmarkStreamRoundTrip(b *testing.B) {
	for _, tr := range benchTransports {
		b.Run(tr.name, func(b *testing.B) {
			for _, pl := range benchPayloads {
				b.Run(pl.name, func(b *testing.B) {
					ctx := drpctest.NewTracker(b)
					conn := tr.create(b, benchEchoServer, ctx)
					defer func() {
						_ = conn.Close()
						ctx.Close()
					}()
					cli := NewDRPCServiceClient(conn)

					stream, err := cli.Method4(ctx)
					assert.NoError(b, err)

					in := &In{In: 1, Data: pl.data}
					b.SetBytes(int64(proto.Size(in)))
					b.ReportAllocs()
					b.ResetTimer()

					for i := 0; i < b.N; i++ {
						if err := stream.Send(in); err != nil {
							b.Fatal(err)
						}
						if _, err := stream.Recv(); err != nil {
							b.Fatal(err)
						}
					}

					b.StopTimer()
					assert.NoError(b, stream.CloseSend())
				})
			}
		})
	}
}

// BenchmarkStreamThroughput measures one-way client-to-server send throughput.
// The client sends b.N messages on a client-streaming RPC (Method2) without
// waiting for per-message responses. This lets the MuxWriter batch multiple
// frames per transport write, showing the maximum achievable send rate.
func BenchmarkStreamThroughput(b *testing.B) {
	for _, tr := range benchTransports {
		b.Run(tr.name, func(b *testing.B) {
			for _, pl := range benchPayloads {
				b.Run(pl.name, func(b *testing.B) {
					ctx := drpctest.NewTracker(b)
					conn := tr.create(b, benchEchoServer, ctx)
					defer func() {
						_ = conn.Close()
						ctx.Close()
					}()
					cli := NewDRPCServiceClient(conn)

					stream, err := cli.Method2(ctx)
					assert.NoError(b, err)

					in := &In{In: 1, Data: pl.data}
					b.SetBytes(int64(proto.Size(in)))
					b.ReportAllocs()
					b.ResetTimer()

					for i := 0; i < b.N; i++ {
						if err := stream.Send(in); err != nil {
							b.Fatal(err)
						}
					}

					b.StopTimer()
					_, err = stream.CloseAndRecv()
					assert.NoError(b, err)
				})
			}
		})
	}
}

// BenchmarkStreamRecvThroughput measures one-way server-to-client receive
// throughput. The server sends b.N messages via Method3 (server-streaming), and
// the client receives them. This exercises the server-side MuxWriter and the
// client-side manageReader dispatch + packetQueue delivery, complementing
// BenchmarkStreamThroughput which measures the send direction.
func BenchmarkStreamRecvThroughput(b *testing.B) {
	for _, tr := range benchTransports {
		b.Run(tr.name, func(b *testing.B) {
			for _, pl := range benchPayloads {
				b.Run(pl.name, func(b *testing.B) {
					ctx := drpctest.NewTracker(b)
					conn := tr.create(b, benchEchoServer, ctx)
					defer func() {
						_ = conn.Close()
						ctx.Close()
					}()
					cli := NewDRPCServiceClient(conn)

					in := &In{In: int64(b.N), Data: pl.data}
					stream, err := cli.Method3(ctx, in)
					if err != nil {
						b.Fatal(err)
					}

					b.ReportAllocs()
					b.ResetTimer()

					var last *Out
					for i := 0; i < b.N; i++ {
						last, err = stream.Recv()
						if err != nil {
							b.Fatal(err)
						}
					}
					if last != nil {
						b.SetBytes(int64(proto.Size(last)))
					}
				})
			}
		})
	}
}

// BenchmarkConcurrentUnary measures how unary RPC throughput scales with
// concurrent callers on a single connection. All goroutines share the same
// MuxWriter and activeStreams map, so this reveals lock contention under load.
// On Pipe, contention is amplified because the MuxWriter's drain goroutine
// blocks synchronously in Write. On TCP, kernel buffering masks contention.
func BenchmarkConcurrentUnary(b *testing.B) {
	for _, tr := range benchTransports {
		b.Run(tr.name, func(b *testing.B) {
			for _, p := range []int{1, 10, 50, 100} {
				b.Run(fmt.Sprintf("P%d", p), func(b *testing.B) {
					ctx := drpctest.NewTracker(b)
					conn := tr.create(b, benchEchoServer, ctx)
					defer func() {
						_ = conn.Close()
						ctx.Close()
					}()
					cli := NewDRPCServiceClient(conn)

					in := &In{In: 1}
					b.ReportAllocs()
					b.ResetTimer()

					var completed atomic.Int64
					var wg sync.WaitGroup
					wg.Add(p)
					for g := 0; g < p; g++ {
						go func() {
							defer wg.Done()
							for completed.Add(1) <= int64(b.N) {
								_, err := cli.Method1(ctx, in)
								if err != nil {
									b.Error(err)
									return
								}
							}
						}()
					}
					wg.Wait()
				})
			}
		})
	}
}

// BenchmarkConcurrentStreams measures multiplexing overhead with S established
// bidi streams on one connection. Each stream does echo round trips. The
// manageReader goroutine is the serial bottleneck (one reader dispatching to
// S packetQueues), while the MuxWriter batches WriteFrame calls from S
// concurrent senders. This shows whether per-stream throughput degrades as
// stream count grows.
func BenchmarkConcurrentStreams(b *testing.B) {
	for _, tr := range benchTransports {
		b.Run(tr.name, func(b *testing.B) {
			for _, s := range []int{1, 10, 50} {
				b.Run(fmt.Sprintf("S%d", s), func(b *testing.B) {
					ctx := drpctest.NewTracker(b)
					conn := tr.create(b, benchEchoServer, ctx)
					defer func() {
						_ = conn.Close()
						ctx.Close()
					}()
					cli := NewDRPCServiceClient(conn)

					streams := make([]DRPCService_Method4Client, s)
					for i := range streams {
						st, err := cli.Method4(ctx)
						assert.NoError(b, err)
						streams[i] = st
					}

					in := &In{In: 1}
					b.ReportAllocs()
					b.ResetTimer()

					var completed atomic.Int64
					var wg sync.WaitGroup
					wg.Add(s)
					for i := range streams {
						st := streams[i]
						go func() {
							defer wg.Done()
							for completed.Add(1) <= int64(b.N) {
								if err := st.Send(in); err != nil {
									b.Error(err)
									return
								}
								if _, err := st.Recv(); err != nil {
									b.Error(err)
									return
								}
							}
						}()
					}
					wg.Wait()

					b.StopTimer()
					for _, st := range streams {
						_ = st.CloseSend()
					}
				})
			}
		})
	}
}

// BenchmarkStreamCreation measures the fixed per-stream lifecycle cost: open a
// bidi stream, exchange one message, close, and wait for EOF. This isolates the
// stream creation tax (stream ID allocation, activeStreams bookkeeping, invoke
// handshake, manageStream goroutine spawn/exit) from the per-message cost.
func BenchmarkStreamCreation(b *testing.B) {
	for _, tr := range benchTransports {
		b.Run(tr.name, func(b *testing.B) {
			ctx := drpctest.NewTracker(b)
			conn := tr.create(b, benchEchoServer, ctx)
			defer func() {
				_ = conn.Close()
				ctx.Close()
			}()
			cli := NewDRPCServiceClient(conn)

			in := &In{In: 1}
			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				stream, err := cli.Method4(ctx)
				if err != nil {
					b.Fatal(err)
				}
				if err := stream.Send(in); err != nil {
					b.Fatal(err)
				}
				if _, err := stream.Recv(); err != nil {
					b.Fatal(err)
				}
				if err := stream.CloseSend(); err != nil {
					b.Fatal(err)
				}
				if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
					b.Fatalf("expected EOF, got %v", err)
				}
			}
		})
	}
}