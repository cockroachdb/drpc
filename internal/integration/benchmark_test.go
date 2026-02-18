// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package integration

import (
	"context"
	"errors"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"storj.io/drpc/drpcconn"
	"storj.io/drpc/drpctest"
)

var echoServer = impl{
	Method1Fn: func(_ context.Context, in *In) (*Out, error) {
		return &Out{Out: in.In, Data: in.Data}, nil
	},

	Method2Fn: func(stream DRPCService_Method2Stream) error {
		var last *In
		for {
			in, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return err
			}
			last = in
		}
		if last == nil {
			last = &In{}
		}
		return stream.SendAndClose(&Out{Out: last.In, Data: last.Data})
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
			in, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				return nil
			} else if err != nil {
				return err
			}
			if err := stream.Send(&Out{Out: in.In, Data: in.Data}); err != nil {
				return err
			}
		}
	},
}

var sizes = []struct {
	name string
	data []byte
}{
	{"small", nil},
	{"1KB", make([]byte, 1024)},
	{"8KB", make([]byte, 8192)},
}

var concurrencies = []int{1, 10, 100}
var activeStreamCounts = []int{2, 8, 32}

const parallelActiveStreamsWriteLatency = 200 * time.Microsecond

func BenchmarkUnary(b *testing.B) {
	for _, sz := range sizes {
		b.Run("size="+sz.name, func(b *testing.B) {
			for _, c := range concurrencies {
				b.Run("concurrent="+strconv.Itoa(c), func(b *testing.B) {
					client, cleanup := createConnection(b, echoServer)
					defer cleanup()

					in := &In{In: 1, Data: sz.data}
					b.SetBytes(int64(proto.Size(in)))
					b.ReportAllocs()
					b.SetParallelism(c)
					b.ResetTimer()

					b.RunParallel(func(pb *testing.PB) {
						for pb.Next() {
							if _, err := client.Method1(context.Background(), in); err != nil {
								b.Error(err)
							}
						}
					})
				})
			}
		})
	}
}

func BenchmarkInputStream(b *testing.B) {
	for _, sz := range sizes {
		b.Run("size="+sz.name, func(b *testing.B) {
			client, cleanup := createConnection(b, echoServer)
			defer cleanup()

			in := &In{In: 1, Data: sz.data}
			stream, err := client.Method2(context.Background())
			if err != nil {
				b.Fatal(err)
			}

			b.SetBytes(int64(proto.Size(in)))
			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				if err := stream.Send(in); err != nil {
					b.Fatal(err)
				}
			}

			if _, err := stream.CloseAndRecv(); err != nil {
				b.Fatal(err)
			}
		})
	}
}

func BenchmarkOutputStream(b *testing.B) {
	for _, sz := range sizes {
		b.Run("size="+sz.name, func(b *testing.B) {
			client, cleanup := createConnection(b, echoServer)
			defer cleanup()

			in := &In{In: int64(b.N), Data: sz.data}
			stream, err := client.Method3(context.Background(), in)
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
}

func BenchmarkBidiStream(b *testing.B) {
	for _, sz := range sizes {
		b.Run("size="+sz.name, func(b *testing.B) {
			client, cleanup := createConnection(b, echoServer)
			defer cleanup()

			in := &In{In: 1, Data: sz.data}
			stream, err := client.Method4(context.Background())
			if err != nil {
				b.Fatal(err)
			}

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
		})
	}
}

func BenchmarkParallelActiveStreams(b *testing.B) {
	benchmarkParallelActiveStreamsModes(b, 0)
}

func BenchmarkParallelActiveStreamsWriteLatency(b *testing.B) {
	benchmarkParallelActiveStreamsModes(b, parallelActiveStreamsWriteLatency)
}

func benchmarkParallelActiveStreamsModes(b *testing.B, writeLatency time.Duration) {
	modes := []struct {
		name             string
		oneConnPerStream bool
		useTCP           bool
	}{
		{name: "mux_single_connection", oneConnPerStream: false},
		{name: "one_connection_per_stream", oneConnPerStream: true},
		{name: "mux_tcp", oneConnPerStream: false, useTCP: true},
		{name: "one_conn_tcp", oneConnPerStream: true, useTCP: true},
	}

	for _, sz := range sizes {
		b.Run("size="+sz.name, func(b *testing.B) {
			for _, streamCount := range activeStreamCounts {
				b.Run("streams="+strconv.Itoa(streamCount), func(b *testing.B) {
					for _, mode := range modes {
						b.Run(mode.name, func(b *testing.B) {
							benchmarkParallelActiveStreams(b, sz.data, streamCount, mode.oneConnPerStream, writeLatency, mode.useTCP)
						})
					}
				})
			}
		})
	}
}

func benchmarkParallelActiveStreams(
	b *testing.B, payload []byte, streamCount int, oneConnPerStream bool, writeLatency time.Duration, useTCP bool,
) {
	ctx := drpctest.NewTracker(b)
	defer ctx.Close()

	type bidiClient interface {
		Send(*In) error
		Recv() (*Out, error)
		Close() error
	}

	type worker struct {
		stream bidiClient
		close  func()
	}
	workers := make([]worker, 0, streamCount)

	createConn := createRawConnectionWithWriteDelay
	if useTCP {
		createConn = createTCPConnectionWithWriteDelay
	}

	var sharedConn *drpcconn.Conn
	if !oneConnPerStream {
		sharedConn = createConn(b, echoServer, ctx, writeLatency)
	}

	for i := 0; i < streamCount; i++ {
		conn := sharedConn
		if oneConnPerStream {
			conn = createConn(b, echoServer, ctx, writeLatency)
		}

		client := NewDRPCServiceClient(conn)
		stream, err := client.Method4(context.Background())
		if err != nil {
			b.Fatalf("create stream %d: %v", i, err)
		}

		s := stream
		c := conn
		workers = append(workers, worker{
			stream: s,
			close: func() {
				_ = s.Close()
				if oneConnPerStream {
					_ = c.Close()
				}
			},
		})
	}

	input := &In{In: 1, Data: payload}
	b.SetBytes(int64(proto.Size(input)))
	b.ReportAllocs()

	var sent atomic.Int64
	errCh := make(chan error, 1)
	start := make(chan struct{})
	var wg sync.WaitGroup

	b.ResetTimer()
	for _, w := range workers {
		wg.Add(1)
		go func(w worker) {
			defer wg.Done()
			msg := &In{In: 1, Data: payload}
			<-start

			for {
				n := sent.Add(1)
				if n > int64(b.N) {
					return
				}
				if err := w.stream.Send(msg); err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
				if _, err := w.stream.Recv(); err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
			}
		}(w)
	}
	close(start)
	wg.Wait()
	b.StopTimer()

	select {
	case err := <-errCh:
		b.Fatal(err)
	default:
	}

	for _, w := range workers {
		w.close()
	}
	if sharedConn != nil {
		_ = sharedConn.Close()
	}
}
