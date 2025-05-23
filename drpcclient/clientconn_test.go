package drpcclient

import (
	"context"
	"github.com/stretchr/testify/assert"
	"storj.io/drpc"
	"storj.io/drpc/drpcpool"
	"storj.io/drpc/drpctest"
	"testing"
	"time"
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

// TestUnaryInterceptorChainWithPooledAndConcreteDrpcConn verifies that unary interceptors
// work correctly with both direct drpcconn.Conn and pooled drpcpool.Conn connection types.
//
// This test ensures that:
// 1. The interceptor chain executes properly in both connection scenarios
// 2. Interceptors are called in the correct order (first-to-last on the way in, last-to-first on the way out)
// 3. The RPC payload is correctly transmitted through the interceptor chain
func TestUnaryInterceptorChainWithPooledAndConcreteDrpcConn(t *testing.T) {
	ctx := drpctest.NewTracker(t)

	// Test cases for different connection supplier implementations
	testCases := []struct {
		name   string
		dialer func(context.Context) (drpc.Conn, error)
	}{
		{
			name: "drpc_connection",
			// Basic connection supplier that returns a concrete connection directly
			dialer: func(context.Context) (drpc.Conn, error) {
				return &mockDrpcConn{}, nil
			},
		},
		{
			name: "drpc_pooled_connection",
			// Pool-based connection supplier that returns connections from a connection pool
			dialer: func(context.Context) (drpc.Conn, error) {
				pool := drpcpool.New[string, drpcpool.Conn](drpcpool.Options{
					Capacity:    2,
					KeyCapacity: 1,
					Expiration:  time.Minute,
				})
				t.Cleanup(func() {
					pool.Close()
				})
				// Get a connection from the pool using a test server address
				return pool.Get(ctx, "test.server:8080", func(ctx context.Context, addr string) (drpcpool.Conn, error) {
					return &mockDrpcConn{}, nil
				}), nil
			},
		},
	}

	for _, tc := range testCases {
		var interceptorCalls []string

		interceptor1 := recordUnaryInterceptor("interceptor1", &interceptorCalls)
		interceptor2 := recordUnaryInterceptor("interceptor2", &interceptorCalls)
		in, out := "foobar", ""
		cc, _ := NewClientConnWithOptions(ctx, tc.dialer, WithChainUnaryInterceptor(interceptor1, interceptor2))
		assert.NoError(t, cc.Invoke(ctx, "TestMethod", testEncoding{}, &in, &out))
		assert.Equal(t, out, "mocked response for request: "+in)

		// Check the order of interceptor calls
		expected := []string{
			"interceptor1_before",
			"interceptor2_before",
			"interceptor2_after",
			"interceptor1_after",
		}
		assert.Equal(t, expected, interceptorCalls)
	}
}

func TestInvokeWithNoInterceptors(t *testing.T) {
	ctx := drpctest.NewTracker(t)

	// Connection dialer that returns a mock connection directly
	dialer := func(ctx2 context.Context) (drpc.Conn, error) {
		return &mockDrpcConn{}, nil
	}

	in, out := "foobar", ""
	cc, _ := NewClientConnWithOptions(ctx, dialer, WithChainUnaryInterceptor())
	assert.NoError(t, cc.Invoke(ctx, "TestMethod", testEncoding{}, &in, &out))
	assert.True(t, out == "mocked response for request: "+in)
}

func TestChainStreamClientInterceptors(t *testing.T) {
	ctx := drpctest.NewTracker(t)

	// Mock connection dialer
	dialer := func(ctx2 context.Context) (drpc.Conn, error) {
		return &mockDrpcConn{}, nil
	}

	var interceptorCalls []string

	// Define interceptors

	interceptor1 := recordStreamInterceptor("interceptor1", &interceptorCalls)
	interceptor2 := recordStreamInterceptor("interceptor2", &interceptorCalls)

	// Create ClientConn using NewClientConnWithOptions
	cc, err := NewClientConnWithOptions(ctx, dialer, WithChainStreamInterceptor(interceptor1, interceptor2))
	assert.NoError(t, err)

	// Invoke the chained interceptor
	_, err = cc.NewStream(ctx, "TestRPC", testEncoding{})
	assert.NoError(t, err)
	// Check the order of interceptor calls
	expected := []string{
		"interceptor1_before",
		"interceptor2_before",
		"interceptor2_after",
		"interceptor1_after",
	}
	assert.Equal(t, expected, interceptorCalls)
}

func recordUnaryInterceptor(name string, calls *[]string) UnaryClientInterceptor {
	return func(ctx context.Context, method string, enc drpc.Encoding,
		in, out drpc.Message, conn *ClientConn, invoker UnaryInvoker) error {
		*calls = append(*calls, name+"_before")
		err := invoker(ctx, method, enc, in, out, conn)
		*calls = append(*calls, name+"_after")
		return err
	}
}

func recordStreamInterceptor(name string, calls *[]string) StreamClientInterceptor {
	return func(ctx context.Context, rpc string, enc drpc.Encoding, conn *ClientConn, next Streamer) (drpc.Stream, error) {
		*calls = append(*calls, name+"_before")
		stream, err := next(ctx, rpc, enc, conn)
		if err == nil {
			*calls = append(*calls, name+"_after")
		}
		return stream, err
	}
}

type mockDrpcConn struct{}

func (m *mockDrpcConn) Unblocked() <-chan struct{} {
	return nil
}

func (m *mockDrpcConn) Invoke(ctx context.Context, rpc string, enc drpc.Encoding, in, out drpc.Message) error {
	*out.(*string) = "mocked response for request: " + *in.(*string)
	return nil
}

func (m *mockDrpcConn) NewStream(ctx context.Context, rpc string, enc drpc.Encoding) (drpc.Stream, error) {
	return &mockStream{name: rpc}, nil
}

func (m *mockDrpcConn) Close() error {
	return nil
}

func (m *mockDrpcConn) Closed() <-chan struct{} {
	return nil
}

type mockStream struct {
	name string
}

func (m *mockStream) MsgSend(msg drpc.Message, enc drpc.Encoding) error {
	return nil
}

func (m *mockStream) MsgRecv(msg drpc.Message, enc drpc.Encoding) error {
	return nil
}

func (m *mockStream) Close() error {
	return nil
}

func (m *mockStream) Context() context.Context {
	return context.TODO()
}

func (m *mockStream) CloseSend() error {
	return nil
}
