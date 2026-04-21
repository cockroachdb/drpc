//go:build bench_connperstream

// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

// This file measures the conn-per-stream strategy: N TCP connections, each
// carrying one stream. Copy this file to a non-mux branch and run:
//
//	go test -tags=bench_connperstream -run='^$' -bench=BenchmarkComparison -benchtime=1s -count=5

package integration

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"storj.io/drpc/drpcconn"
	"storj.io/drpc/drpctest"
)

var comparisonServer = impl{
	Method1Fn: func(_ context.Context, in *In) (*Out, error) {
		return &Out{Out: in.In, Data: in.Data}, nil
	},
	Method2Fn: nil,
	Method3Fn: nil,
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

// BenchmarkComparison measures N concurrent RPCs over TCP using the
// conn-per-stream strategy: each worker gets its own TCP connection.
// Run the matching bench_mux file on the mux branch and compare with benchstat.
func BenchmarkComparison(b *testing.B) {
	streamCounts := []int{1, 4, 16, 64}

	b.Run("Bidi", func(b *testing.B) {
		for _, n := range streamCounts {
			b.Run(fmt.Sprintf("N%d", n), func(b *testing.B) {
				ctx := drpctest.NewTracker(b)
				type worker struct {
					stream DRPCService_Method4Client
					conn   *drpcconn.Conn
				}
				workers := make([]worker, n)
				for i := range workers {
					conn := createTCPConnection(b, comparisonServer, ctx)
					cli := NewDRPCServiceClient(conn)
					st, err := cli.Method4(ctx)
					if err != nil {
						b.Fatal(err)
					}
					workers[i] = worker{stream: st, conn: conn}
				}

				in := &In{In: 1}
				b.ReportAllocs()
				b.ResetTimer()

				var completed atomic.Int64
				var wg sync.WaitGroup
				wg.Add(n)
				for i := range workers {
					w := workers[i]
					go func() {
						defer wg.Done()
						for completed.Add(1) <= int64(b.N) {
							if err := w.stream.Send(in); err != nil {
								b.Error(err)
								return
							}
							if _, err := w.stream.Recv(); err != nil {
								b.Error(err)
								return
							}
						}
					}()
				}
				wg.Wait()

				b.StopTimer()
				for _, w := range workers {
					_ = w.conn.Close()
				}
				ctx.Close()
			})
		}
	})

	b.Run("Unary", func(b *testing.B) {
		for _, n := range streamCounts {
			b.Run(fmt.Sprintf("N%d", n), func(b *testing.B) {
				ctx := drpctest.NewTracker(b)
				type worker struct {
					cli  DRPCServiceClient
					conn *drpcconn.Conn
				}
				workers := make([]worker, n)
				for i := range workers {
					conn := createTCPConnection(b, comparisonServer, ctx)
					workers[i] = worker{
						cli:  NewDRPCServiceClient(conn),
						conn: conn,
					}
				}

				in := &In{In: 1}
				b.ReportAllocs()
				b.ResetTimer()

				var completed atomic.Int64
				var wg sync.WaitGroup
				wg.Add(n)
				for i := range workers {
					w := workers[i]
					go func() {
						defer wg.Done()
						for completed.Add(1) <= int64(b.N) {
							if _, err := w.cli.Method1(ctx, in); err != nil {
								b.Error(err)
								return
							}
						}
					}()
				}
				wg.Wait()

				b.StopTimer()
				for _, w := range workers {
					_ = w.conn.Close()
				}
				ctx.Close()
			})
		}
	})
}
