package drpcserver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"storj.io/drpc"
)

// emptyMessage is a drpc.Message that carries no data.
type emptyMessage struct{}

// DRPCMessage implements drpc.Message to satisfy the interface.
func (emptyMessage) DRPCMessage() {}

// dummyEncoding is a no-op drpc.Encoding for testing purposes.
type dummyEncoding struct{}

// Marshal implements drpc.Encoding.
func (d dummyEncoding) Marshal(msg drpc.Message) ([]byte, error) {
	if msg == nil {
		return nil, nil
	}
	// For an emptyMessage, we can return an empty byte slice.
	if _, ok := msg.(*emptyMessage); ok {
		return []byte{}, nil
	}
	return nil, errors.New("dummyEncoding can only marshal *emptyMessage")
}

// Unmarshal implements drpc.Encoding.
func (d dummyEncoding) Unmarshal(data []byte, msg drpc.Message) error {
	return nil
}

// mockHandler is a mock drpc.Handler.
type mockHandler struct {
	fn func(stream drpc.Stream, rpc string) error
}

func (m *mockHandler) HandleRPC(stream drpc.Stream, rpc string) error {
	if m.fn != nil {
		return m.fn(stream, rpc)
	}
	return nil
}

// mockStream is a mock drpc.Stream.
type mockStream struct {
	ctx context.Context

	// Fields to track behavior for tests
	msgSent         []drpc.Message
	msgRecvd        []drpc.Message
	closeSendCalled bool
	closedCalled    bool
	sendErrorCalled bool
	lastErrorSent   error
}

func (m *mockStream) Context() context.Context {
	if m.ctx != nil {
		return m.ctx
	}
	return context.Background()
}

func (m *mockStream) MsgSend(msg drpc.Message, _ drpc.Encoding) error {
	m.msgSent = append(m.msgSent, msg)
	return nil
}

func (m *mockStream) MsgRecv(_ drpc.Message, _ drpc.Encoding) error {
	if len(m.msgRecvd) > 0 {
		return nil
	}
	return errors.New("mockStream: no messages to receive")
}

func (m *mockStream) CloseSend() error {
	m.closeSendCalled = true
	return nil
}

func (m *mockStream) Close() error {
	m.closedCalled = true
	return nil
}

func (m *mockStream) SendError(err error) error {
	m.sendErrorCalled = true
	m.lastErrorSent = err
	return nil
}

func TestWithChainServerInterceptor(t *testing.T) {
	interceptor1 := func(ctx context.Context, rpc string, stream drpc.Stream, handler drpc.Handler) error {
		return handler.HandleRPC(stream, rpc)
	}
	interceptor2 := func(ctx context.Context, rpc string, stream drpc.Stream, handler drpc.Handler) error {
		return handler.HandleRPC(stream, rpc)
	}

	opt := WithChainServerInterceptor(interceptor1, interceptor2)
	opts := &Options{}
	opt(opts)

	if opts.serverInts == nil || len(opts.serverInts) != 2 {
		t.Fatal("serverInts should not be nil")
	}
}

func TestNewWithOptions_WithInterceptors(t *testing.T) {
	interceptor := func(ctx context.Context, rpc string, stream drpc.Stream, handler drpc.Handler) error {
		return handler.HandleRPC(stream, rpc)
	}
	// Create a mock handler instance
	mockRPCHandler := &mockHandler{
		fn: func(stream drpc.Stream, rpc string) error {
			return nil
		},
	}
	srv := NewWithOptions(mockRPCHandler, Options{}, WithChainServerInterceptor(interceptor))

	if srv.opts.serverInt == nil {
		t.Fatal("serverInt should not be nil in server options")
	}
}

func TestServer_handleRPC_InterceptorError(t *testing.T) {
	expectedErr := errors.New("interceptor error")

	interceptor1 := func(ctx context.Context, rpc string, stream drpc.Stream, handler drpc.Handler) error {
		return expectedErr // Error out before calling next
	}

	interceptor2 := func(ctx context.Context, rpc string, stream drpc.Stream, handler drpc.Handler) error {
		t.Error("interceptor2 should not be called")
		return handler.HandleRPC(stream, rpc)
	}

	handlerCalled := false
	mockRPCHandler := &mockHandler{
		fn: func(stream drpc.Stream, rpc string) error {
			handlerCalled = true
			return nil
		},
	}

	srv := NewWithOptions(mockRPCHandler, Options{}, WithChainServerInterceptor(interceptor1, interceptor2))
	finalInterceptor := srv.opts.serverInt
	if finalInterceptor == nil {
		t.Fatal("serverInt is nil after NewWithOptions")
	}

	err := finalInterceptor(context.Background(), "TestRPC", &mockStream{}, mockRPCHandler)
	if err == nil {
		t.Fatal("expected an error from interceptor chain, got nil")
	}
	if !strings.Contains(err.Error(), expectedErr.Error()) {
		t.Errorf("expected error '%v', got '%v'", expectedErr, err)
	}

	if handlerCalled {
		t.Error("handler should not have been called when an interceptor errors")
	}
}

type contextKey string

const testCtxKey = contextKey("testKey")

func TestServer_handleRPC_InterceptorContextPropagation(t *testing.T) {
	var (
		valFromCtx1 interface{}
		valFromCtx2 interface{}
	)

	interceptor1 := func(ctx context.Context, rpc string, stream drpc.Stream, handler drpc.Handler) error {
		newCtx := context.WithValue(ctx, testCtxKey, "value1")
		return handler.HandleRPC(&mockStream{ctx: newCtx}, rpc) // Pass modified context via stream
	}

	interceptor2 := func(ctx context.Context, rpc string, stream drpc.Stream, handler drpc.Handler) error {
		valFromCtx1 = stream.Context().Value(testCtxKey)
		newCtx := context.WithValue(stream.Context(), testCtxKey, "value2")
		return handler.HandleRPC(&mockStream{ctx: newCtx}, rpc) // Pass modified context via stream
	}

	mockRPCHandler := &mockHandler{
		fn: func(stream drpc.Stream, rpc string) error {
			valFromCtx2 = stream.Context().Value(testCtxKey) // This should be "value2"
			return nil
		},
	}

	srv := NewWithOptions(mockRPCHandler, Options{}, WithChainServerInterceptor(interceptor1, interceptor2))
	finalInterceptor := srv.opts.serverInt
	if finalInterceptor == nil {
		t.Fatal("serverInt is nil after NewWithOptions")
	}

	// We pass the initial context via the mockStream passed to the finalInterceptor
	initialCtx := context.Background()
	err := finalInterceptor(initialCtx, "TestRPC", &mockStream{ctx: initialCtx}, mockRPCHandler)
	if err != nil {
		t.Fatalf("interceptor chain returned an error: %v", err)
	}

	if valFromCtx1 != "value1" {
		t.Errorf("expected value 'value1' from context in interceptor2, got '%v'", valFromCtx1)
	}
	if valFromCtx2 != "value2" {
		t.Errorf("expected value 'value2' from context in handler (set by interceptor2), got '%v'", valFromCtx2)
	}
}

func TestServer_handleRPC_NoInterceptors(t *testing.T) {
	handlerCalled := false
	mockRPCHandler := &mockHandler{
		fn: func(stream drpc.Stream, rpc string) error {
			handlerCalled = true
			return nil
		},
	}

	// Create server without interceptors
	srv := NewWithOptions(mockRPCHandler, Options{})
	if srv.opts.serverInt != nil {
		t.Error("serverInt should be nil when no interceptors are provided")
	}

	// check the behavior of the interceptor application logic
	// when no interceptors are configured.
	opts := &Options{} // Default options, serverInt should be nil

	var effectiveHandler drpc.Handler = mockRPCHandler
	if opts.serverInt != nil {
		// This block should not be hit if no interceptors are set
		err := opts.serverInt(context.Background(), "TestRPC", &mockStream{}, mockRPCHandler)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	} else {
		// This is the expected path if no interceptors are set
		err := effectiveHandler.HandleRPC(&mockStream{}, "TestRPC")
		if err != nil {
			t.Fatalf("handler returned an error: %v", err)
		}
	}

	if !handlerCalled {
		t.Error("handler was not called when no interceptors are present")
	}
}
