package drpcmux

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"storj.io/drpc"
)

type mockMessage struct {
	Value string
}

type mockStream struct {
	ctx     context.Context
	recvMsg drpc.Message
	sendMsg drpc.Message
	closed  bool
}

func (m *mockStream) Context() context.Context {
	return m.ctx
}

func (m *mockStream) Kind() drpc.StreamKind {
	return drpc.StreamKindServer
}

func (m *mockStream) MsgSend(msg drpc.Message, enc drpc.Encoding) error {
	if m.closed {
		return errors.New("stream closed")
	}
	m.sendMsg = msg
	return nil
}

func (m *mockStream) MsgRecv(msg drpc.Message, enc drpc.Encoding) error {
	if m.closed {
		return errors.New("stream closed")
	}
	if m.recvMsg == nil {
		return errors.New("no message to receive")
	}

	// Simple implementation that just copies the mock message to the output message
	if mockMsg, ok := m.recvMsg.(*mockMessage); ok {
		if outMsg, ok := msg.(*mockMessage); ok {
			outMsg.Value = mockMsg.Value
			return nil
		}
	}

	return errors.New("message type mismatch")
}

func (m *mockStream) CloseSend() error {
	m.closed = true
	return nil
}

func (m *mockStream) Close() error {
	m.closed = true
	return nil
}

type mockEncoding struct{}

func (m mockEncoding) Marshal(msg drpc.Message) ([]byte, error) {
	return []byte{}, nil
}

func (m mockEncoding) Unmarshal(data []byte, msg drpc.Message) error {
	return nil
}

// TestChainUnaryInterceptors tests that unary interceptors are chained and
// executed in the correct order
func TestChainUnaryInterceptors(t *testing.T) {
	r := require.New(t)
	var order []string

	interceptor1 := func(ctx context.Context, req any, rpc string, handler UnaryHandler) (any, error) {
		order = append(order, "interceptor1_before")
		resp, err := handler(ctx, req)
		order = append(order, "interceptor1_after")
		return resp, err
	}

	interceptor2 := func(ctx context.Context, req any, rpc string, handler UnaryHandler) (any, error) {
		order = append(order, "interceptor2_before")
		resp, err := handler(ctx, req)
		order = append(order, "interceptor2_after")
		return resp, err
	}

	interceptor3 := func(ctx context.Context, req any, rpc string, handler UnaryHandler) (any, error) {
		order = append(order, "interceptor3_before")
		resp, err := handler(ctx, req)
		order = append(order, "interceptor3_after")
		return resp, err
	}

	finalHandler := func(ctx context.Context, req any) (any, error) {
		order = append(order, "handler")
		return &mockMessage{Value: "response"}, nil
	}

	interceptor := chainUnaryInterceptors([]UnaryServerInterceptor{interceptor1, interceptor2, interceptor3})

	resp, err := interceptor(context.Background(), &mockMessage{Value: "request"}, "test.rpc", finalHandler)
	r.NoError(err)

	mockResp, ok := resp.(*mockMessage)
	r.True(ok, "expected *mockMessage, got %T", resp)
	r.Equal("response", mockResp.Value)

	expectedOrder := []string{
		"interceptor1_before",
		"interceptor2_before",
		"interceptor3_before",
		"handler",
		"interceptor3_after",
		"interceptor2_after",
		"interceptor1_after",
	}

	r.Equal(expectedOrder, order)
}

// TestChainStreamInterceptors tests that stream interceptors are chained and
// executed in the correct order
func TestChainStreamInterceptors(t *testing.T) {
	r := require.New(t)
	var order []string

	interceptor1 := func(stream drpc.Stream, rpc string, handler StreamHandler) (any, error) {
		order = append(order, "interceptor1_before")
		resp, err := handler(stream)
		order = append(order, "interceptor1_after")
		return resp, err
	}

	interceptor2 := func(stream drpc.Stream, rpc string, handler StreamHandler) (any, error) {
		order = append(order, "interceptor2_before")
		resp, err := handler(stream)
		order = append(order, "interceptor2_after")
		return resp, err
	}

	interceptor3 := func(stream drpc.Stream, rpc string, handler StreamHandler) (any, error) {
		order = append(order, "interceptor3_before")
		resp, err := handler(stream)
		order = append(order, "interceptor3_after")
		return resp, err
	}

	finalHandler := func(stream drpc.Stream) (any, error) {
		order = append(order, "handler")
		return &mockMessage{Value: "response"}, nil
	}

	interceptor := chainStreamInterceptors([]StreamServerInterceptor{interceptor1, interceptor2, interceptor3})

	mockStrm := &mockStream{ctx: context.Background()}
	resp, err := interceptor(mockStrm, "test.rpc", finalHandler)
	r.NoError(err)

	mockResp, ok := resp.(*mockMessage)
	r.True(ok, "expected *mockMessage, got %T", resp)
	r.Equal("response", mockResp.Value)

	expectedOrder := []string{
		"interceptor1_before",
		"interceptor2_before",
		"interceptor3_before",
		"handler",
		"interceptor3_after",
		"interceptor2_after",
		"interceptor1_after",
	}

	r.Equal(expectedOrder, order)
}

// TestChainUnaryInterceptorsWithError tests that errors are properly propagated
// back through the chain
func TestChainUnaryInterceptorsWithError(t *testing.T) {
	r := require.New(t)
	var order []string
	expectedError := errors.New("interceptor error")

	interceptor1 := func(ctx context.Context, req any, rpc string, handler UnaryHandler) (any, error) {
		order = append(order, "interceptor1_before")
		resp, err := handler(ctx, req)
		order = append(order, "interceptor1_after")
		return resp, err
	}

	interceptor2 := func(ctx context.Context, req any, rpc string, handler UnaryHandler) (any, error) {
		order = append(order, "interceptor2_before")
		return nil, expectedError // Abort chain with error
	}

	interceptor3 := func(ctx context.Context, req any, rpc string, handler UnaryHandler) (any, error) {
		order = append(order, "interceptor3_before")
		resp, err := handler(ctx, req)
		order = append(order, "interceptor3_after")
		return resp, err
	}

	finalHandler := func(ctx context.Context, req any) (any, error) {
		order = append(order, "handler")
		return &mockMessage{Value: "response"}, nil
	}

	interceptor := chainUnaryInterceptors([]UnaryServerInterceptor{interceptor1, interceptor2, interceptor3})

	resp, err := interceptor(context.Background(), &mockMessage{Value: "request"}, "test.rpc", finalHandler)

	r.Equal(expectedError, err)
	r.Nil(resp, "expected nil response")

	expectedOrder := []string{
		"interceptor1_before",
		"interceptor2_before",
		"interceptor1_after",
	}

	r.Equal(expectedOrder, order)
}

// TestEmptyInterceptors tests edge cases with empty interceptor slices
func TestEmptyInterceptors(t *testing.T) {
	r := require.New(t)

	emptyUnaryInterceptor := chainUnaryInterceptors(nil)
	r.Nil(emptyUnaryInterceptor, "expected nil for empty unary interceptor chain")

	emptyStreamInterceptor := chainStreamInterceptors(nil)
	r.Nil(emptyStreamInterceptor, "expected nil for empty stream interceptor chain")
}
