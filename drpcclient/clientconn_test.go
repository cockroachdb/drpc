package drpcclient

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"storj.io/drpc"
	"storj.io/drpc/drpctest"
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

// TestUnaryInterceptorChain verifies that unary interceptors
// work correctly with a provided drpc.Conn object.
//
// This test ensures that:
// 1. The interceptor chain executes properly
// 2. Interceptors are called in the correct order (first-to-last on the way in, last-to-first on the way out)
// 3. The RPC payload is correctly transmitted through the interceptor chain
func TestUnaryInterceptorChain(t *testing.T) {
	ctx := drpctest.NewTracker(t)

	var interceptorCalls []string

	interceptor1 := recordUnaryInterceptor("interceptor1", &interceptorCalls)
	interceptor2 := recordUnaryInterceptor("interceptor2", &interceptorCalls)
	in, out := "foobar", ""
	cc, _ := NewClientConnWithOptions(ctx, &mockDrpcConn{}, WithChainUnaryInterceptor(interceptor1, interceptor2))
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

func TestInvokeWithNoInterceptors(t *testing.T) {
	ctx := drpctest.NewTracker(t)

	in, out := "foobar", ""
	cc, _ := NewClientConnWithOptions(ctx, &mockDrpcConn{}, WithChainUnaryInterceptor())
	assert.NoError(t, cc.Invoke(ctx, "TestMethod", testEncoding{}, &in, &out))
	assert.True(t, out == "mocked response for request: "+in)
}

func TestChainStreamClientInterceptors(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	var interceptorCalls []string

	// Define interceptors
	interceptor1 := recordStreamInterceptor("interceptor1", &interceptorCalls)
	interceptor2 := recordStreamInterceptor("interceptor2", &interceptorCalls)

	// Create ClientConn using NewClientConnWithOptions
	cc, err := NewClientConnWithOptions(ctx, &mockDrpcConn{}, WithChainStreamInterceptor(interceptor1, interceptor2))
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
	return func(
		ctx context.Context, method string, enc drpc.Encoding,
		in, out drpc.Message, conn *ClientConn, invoker UnaryInvoker,
	) error {
		*calls = append(*calls, name+"_before")
		err := invoker(ctx, method, enc, in, out, conn)
		*calls = append(*calls, name+"_after")
		return err
	}
}

func recordStreamInterceptor(name string, calls *[]string) StreamClientInterceptor {
	return func(
		ctx context.Context,
		rpc string,
		enc drpc.Encoding,
		conn *ClientConn,
		next Streamer,
	) (drpc.Stream, error) {
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
