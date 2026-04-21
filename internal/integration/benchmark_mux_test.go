//go:build bench_mux

// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

// This file measures the multiplexing strategy: one TCP connection carrying
// N streams. Run on the mux branch:
//
//	go test -tags=bench_mux -run='^$' -bench=BenchmarkComparison -benchtime=1s -count=5

package integration

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

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
// multiplexing strategy: all workers share a single TCP connection.
// Run the matching bench_connperstream file on the non-mux branch and compare
// with benchstat.
func BenchmarkComparison(b *testing.B) {
	streamCounts := []int{1, 4, 16, 64}

	b.Run("Bidi", func(b *testing.B) {
		for _, n := range streamCounts {
			b.Run(fmt.Sprintf("N%d", n), func(b *testing.B) {
				ctx := drpctest.NewTracker(b)
				conn := createTCPConnection(b, comparisonServer, ctx)
				cli := NewDRPCServiceClient(conn)

				streams := make([]DRPCService_Method4Client, n)
				for i := range streams {
					st, err := cli.Method4(ctx)
					if err != nil {
						b.Fatal(err)
					}
					streams[i] = st
				}

				in := &In{In: 1}
				b.ReportAllocs()
				b.ResetTimer()

				var completed atomic.Int64
				var wg sync.WaitGroup
				wg.Add(n)
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
				_ = conn.Close()
				ctx.Close()
			})
		}
	})

	b.Run("Unary", func(b *testing.B) {
		for _, n := range streamCounts {
			b.Run(fmt.Sprintf("N%d", n), func(b *testing.B) {
				ctx := drpctest.NewTracker(b)
				conn := createTCPConnection(b, comparisonServer, ctx)
				cli := NewDRPCServiceClient(conn)

				in := &In{In: 1}
				b.ReportAllocs()
				b.ResetTimer()

				var completed atomic.Int64
				var wg sync.WaitGroup
				wg.Add(n)
				for g := 0; g < n; g++ {
					go func() {
						defer wg.Done()
						for completed.Add(1) <= int64(b.N) {
							if _, err := cli.Method1(ctx, in); err != nil {
								b.Error(err)
								return
							}
						}
					}()
				}
				wg.Wait()

				b.StopTimer()
				_ = conn.Close()
				ctx.Close()
			})
		}
	})
}
