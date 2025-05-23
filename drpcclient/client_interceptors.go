package drpcclient

import (
	"context"
	"storj.io/drpc"
)

// UnaryInvoker is called by UnaryClientInterceptor to execute the actual RPC.
// It is responsible for sending the request message to the server
// and receiving the response from the server.
type UnaryInvoker func(ctx context.Context, rpc string, enc drpc.Encoding, in, out drpc.Message, cc *ClientConn) error

// UnaryClientInterceptor defines a function type for intercepting unary RPC calls on the client side.
// This interceptor allows you to add custom logic before and/or after the execution of a unary RPC.
// It can be used for cross-cutting concerns such as logging, metrics, authentication, or error handling.
//
// Unary interceptors can be added to a ClientConn by passing them as DialOption using the WithChainUnaryInterceptor()
// during client connection setup.
//
// The interceptor must call `next` to proceed with the RPC, unless it intends to short-circuit the call.
// It should return an error compatible with the drpcerr package if the RPC fails.
type UnaryClientInterceptor func(ctx context.Context, rpc string, enc drpc.Encoding, in, out drpc.Message, cc *ClientConn, next UnaryInvoker) error

// Streamer is a function that opens a new DRPC stream.
type Streamer func(ctx context.Context, rpc string, enc drpc.Encoding, cc *ClientConn) (drpc.Stream, error)

// StreamClientInterceptor defines a function type for intercepting streaming RPC calls on the client side.
// This interceptor allows you to add custom logic before and/or after the creation of a streaming RPC.
// It can be used for cross-cutting concerns such as logging, metrics, authentication, or error handling.
//
// Stream interceptors can be added to a ClientConn by passing them as DialOption using the WithChainStreamInterceptor()
// during client connection setup.
//
// The interceptor must call `streamer` to proceed with the RPC, unless it intends to short-circuit the call.
// It should return the stream created by the streamer function or an error if the operation fails. The error should be
// compatible with the drpcerr package.
type StreamClientInterceptor func(ctx context.Context, rpc string, enc drpc.Encoding, cc *ClientConn, streamer Streamer) (drpc.Stream, error)
