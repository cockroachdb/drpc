package drpcserver

import (
	"context"
	"storj.io/drpc"
)

// HandlerFunc is an adapter to allow the use of ordinary functions as drpc.Handlers.
// If f is a function with the appropriate signature, HandlerFunc(f) is a
// drpc.Handler object that calls f.
type HandlerFunc func(stream drpc.Stream, rpc string) error

// HandleRPC calls f(stream, rpc).
// It implements the drpc.Handler interface.
func (f HandlerFunc) HandleRPC(stream drpc.Stream, rpc string) error {
	return f(stream, rpc)
}

// ServerInterceptor is a function that intercepts the execution of a DRPC method on the server.
// It allows for cross-cutting concerns like logging, metrics, authentication, or request manipulation
// to be applied to RPCs.
// It is the responsibility of the interceptor to call handler.HandleRPC to continue
// processing the RPC, or to terminate the RPC by returning an error or handling it directly.
type ServerInterceptor func(ctx context.Context, rpc string, stream drpc.Stream, handler drpc.Handler) error
