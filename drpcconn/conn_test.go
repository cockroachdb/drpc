// Copyright (C) 2021 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcconn

import (
	"context"
	"github.com/stretchr/testify/require"
	"net"
	"storj.io/drpc/drpcinterceptors"
	"testing"
	"time"

	"github.com/zeebo/assert"

	"storj.io/drpc"
	"storj.io/drpc/drpctest"
	"storj.io/drpc/drpcwire"
)

// Dummy encoding, which assumes the drpc.Message is a *string.
type testEncoding struct{}

func (testEncoding) Marshal(msg drpc.Message) ([]byte, error) {
	return []byte(*msg.(*string)), nil
}

func (testEncoding) Unmarshal(buf []byte, msg drpc.Message) error {
	*msg.(*string) = string(buf)
	return nil
}

func TestConn_InvokeFlushesSendClose(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	pc, ps := net.Pipe()
	defer func() { assert.NoError(t, pc.Close()) }()
	defer func() { assert.NoError(t, ps.Close()) }()

	invokeDone := make(chan struct{})

	ctx.Run(func(ctx context.Context) {
		wr := drpcwire.NewWriter(ps, 64)
		rd := drpcwire.NewReader(ps)

		_, _ = rd.ReadPacket()    // Invoke
		_, _ = rd.ReadPacket()    // Message
		pkt, _ := rd.ReadPacket() // CloseSend

		_ = wr.WritePacket(drpcwire.Packet{
			Data: []byte("qux"),
			ID:   drpcwire.ID{Stream: pkt.ID.Stream, Message: 1},
			Kind: drpcwire.KindMessage,
		})
		_ = wr.Flush()

		_, _ = rd.ReadPacket() // Close
		<-invokeDone           // wait for invoke to return

		// ensure that any later packets are dropped by writing one
		// before closing the transport.
		for i := 0; i < 5; i++ {
			_ = wr.WritePacket(drpcwire.Packet{
				ID:   drpcwire.ID{Stream: pkt.ID.Stream, Message: 2},
				Kind: drpcwire.KindCloseSend,
			})
			_ = wr.Flush()
		}

		_ = ps.Close()
	})

	conn := New(pc)

	in, out := "baz", ""
	assert.NoError(t, conn.Invoke(ctx, "/com.example.Foo/Bar", testEncoding{}, &in, &out))
	assert.True(t, out == "qux")

	invokeDone <- struct{}{} // signal invoke has returned

	// we should eventually notice the transport is closed
	select {
	case <-conn.Closed():
	case <-time.After(1 * time.Second):
		t.Fatal("took too long for conn to be closed")
	}
}

func runStreamServerSide(ps net.Conn, t *testing.T, sendMessage bool, done chan struct{}) {
	rd := drpcwire.NewReader(ps)
	wr := drpcwire.NewWriter(ps, 64)
	pkt, _ := rd.ReadPacket() // Invoke

	if sendMessage {
		_ = wr.WritePacket(drpcwire.Packet{
			ID:   pkt.ID,
			Kind: drpcwire.KindMessage,
			Data: []byte{},
		})
		_ = wr.Flush()
	}
	_, _ = rd.ReadPacket() // Close
	close(done)
}

func runUnaryServerSide(ps net.Conn, t *testing.T, response string, done chan struct{}) {
	rd := drpcwire.NewReader(ps)
	wr := drpcwire.NewWriter(ps, 64)
	_, _ = rd.ReadPacket()    // Invoke
	_, _ = rd.ReadPacket()    // Message
	pkt, _ := rd.ReadPacket() // CloseSend

	_ = wr.WritePacket(drpcwire.Packet{
		Data: []byte(response),
		ID:   drpcwire.ID{Stream: pkt.ID.Stream, Message: 1},
		Kind: drpcwire.KindMessage,
	})
	_ = wr.Flush()
	_, _ = rd.ReadPacket() // Close
	close(done)
}

func TestInterceptors_TableDriven(t *testing.T) {
	type testCase struct {
		name               string
		unaryInterceptors  []drpcinterceptors.UnaryClientInterceptor
		streamInterceptors []drpcinterceptors.StreamClientInterceptor
		expectUnaryCalls   []string
		expectStreamCalls  []string
		expectUnaryResult  string
		expectUnaryError   bool
		expectStreamError  bool
		serverFunc         func(ps net.Conn, t *testing.T, done chan struct{})
	}

	cases := []testCase{
		{
			name: "unary interceptor chain executes in correct order",
			unaryInterceptors: []drpcinterceptors.UnaryClientInterceptor{
				func(ctx context.Context, method string, in, out drpc.Message, conn drpc.Conn, enc drpc.Encoding, invoker drpcinterceptors.UnaryInvoker) error {
					calls := ctx.Value("calls").(*[]string)
					*calls = append(*calls, "interceptor1_before")
					err := invoker(ctx, method, in, out, enc)
					*calls = append(*calls, "interceptor1_after")
					return err
				},
				func(ctx context.Context, method string, in, out drpc.Message, conn drpc.Conn, enc drpc.Encoding, invoker drpcinterceptors.UnaryInvoker) error {
					calls := ctx.Value("calls").(*[]string)
					*calls = append(*calls, "interceptor2_before")
					err := invoker(ctx, method, in, out, enc)
					*calls = append(*calls, "interceptor2_after")
					return err
				},
			},
			expectUnaryCalls:  []string{"interceptor1_before", "interceptor2_before", "interceptor2_after", "interceptor1_after"},
			expectUnaryResult: "output",
			serverFunc: func(ps net.Conn, t *testing.T, done chan struct{}) {
				runUnaryServerSide(ps, t, "output", done)
			},
		},
		{
			name: "stream interceptor chain executes in correct order",
			streamInterceptors: []drpcinterceptors.StreamClientInterceptor{
				func(ctx context.Context, method string, conn drpc.Conn, streamer drpcinterceptors.Streamer) (drpc.Stream, error) {
					calls := ctx.Value("calls").(*[]string)
					*calls = append(*calls, "streamInterceptor1_before")
					stream, err := streamer(ctx, method)
					*calls = append(*calls, "streamInterceptor1_after")
					return stream, err
				},
				func(ctx context.Context, method string, conn drpc.Conn, streamer drpcinterceptors.Streamer) (drpc.Stream, error) {
					calls := ctx.Value("calls").(*[]string)
					*calls = append(*calls, "streamInterceptor2_before")
					stream, err := streamer(ctx, method)
					*calls = append(*calls, "streamInterceptor2_after")
					return stream, err
				},
			},
			expectStreamCalls: []string{"streamInterceptor1_before", "streamInterceptor2_before", "streamInterceptor2_after", "streamInterceptor1_after"},
			serverFunc: func(ps net.Conn, t *testing.T, done chan struct{}) {
				runStreamServerSide(ps, t, true, done)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := drpctest.NewTracker(t)
			pc, ps := net.Pipe()
			defer pc.Close()
			defer ps.Close()

			var unaryCalls, streamCalls []string
			unaryCalled := false
			streamCalled := false

			ctxVal := context.WithValue(ctx, "calls", &unaryCalls)
			ctxVal = context.WithValue(ctxVal, "unaryCalled", &unaryCalled)
			ctxVal = context.WithValue(ctxVal, "streamCalled", &streamCalled)

			dialOpts := drpcinterceptors.NewDialOptions(nil)
			if len(tc.unaryInterceptors) > 0 {
				dialOpts = drpcinterceptors.NewDialOptions([]drpcinterceptors.DialOption{
					drpcinterceptors.WithChainUnaryInterceptor(tc.unaryInterceptors...),
				})
			}
			if len(tc.streamInterceptors) > 0 {
				dialOpts = drpcinterceptors.NewDialOptions([]drpcinterceptors.DialOption{
					drpcinterceptors.WithChainStreamInterceptor(tc.streamInterceptors...),
				})
			}

			opts := Options{
				dopts: *dialOpts,
			}
			conn := NewWithOptions(pc, opts)
			done := make(chan struct{})

			ctx.Run(func(ctx context.Context) {
				tc.serverFunc(ps, t, done)
			})

			if len(tc.unaryInterceptors) > 0 {
				in, out := "input", ""
				err := conn.Invoke(ctxVal, "TestMethod", testEncoding{}, &in, &out)
				if tc.expectUnaryError {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
					assert.Equal(t, tc.expectUnaryResult, out)
					if len(tc.expectUnaryCalls) > 0 {
						assert.Equal(t, tc.expectUnaryCalls, unaryCalls)
					}
				}
			}

			if len(tc.streamInterceptors) > 0 {
				ctxVal = context.WithValue(ctx, "calls", &streamCalls)
				stream, err := conn.NewStream(ctxVal, "TestStreamMethod", testEncoding{})
				if tc.expectStreamError {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
					stream.Close()
					if len(tc.expectStreamCalls) > 0 {
						assert.Equal(t, tc.expectStreamCalls, streamCalls)
					}
				}
			}

			if tc.name == "mixed interceptors work together" {
				assert.True(t, unaryCalled)
				assert.True(t, streamCalled)
			}

			<-done
		})
	}
}
