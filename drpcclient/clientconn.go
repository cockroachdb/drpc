package drpcclient

import (
	"context"
	"storj.io/drpc"
)

// DialerFunc is a function that returns a drpc.Conn or an error.
type DialerFunc func(ctx context.Context) (drpc.Conn, error)

// ClientConn represents a DRPC client connection, with support for configuring the
// connection with dial options such as interceptors.
type ClientConn struct {
	drpc.Conn
	dopts dialOptions
}

// NewClientConnWithOptions creates a new ClientConn with the specified dial options
// and dialer function. The dialer function is used to obtain the underlying drpc.Conn,
// either from a pool or a concrete connection.
func NewClientConnWithOptions(ctx context.Context, dialer DialerFunc, opts ...DialOption) (*ClientConn, error) {
	conn, err := dialer(ctx)
	if err != nil {
		return nil, err
	}

	clientConn := &ClientConn{
		Conn:  conn,
		dopts: defaultDialOptions(),
	}
	for _, opt := range opts {
		opt(&clientConn.dopts)
	}
	clientConn.initInterceptors()
	return clientConn, nil
}

// finalInvoker returns a UnaryInvoker which executes at the end in an interceptor chain.
func finalInvoker(ctx context.Context, rpc string, enc drpc.Encoding, in, out drpc.Message, cc *ClientConn) error {
	return cc.Conn.Invoke(ctx, rpc, enc, in, out)
}

func (c *ClientConn) Invoke(ctx context.Context, rpc string, enc drpc.Encoding, in, out drpc.Message) error {
	if c.dopts.unaryInt != nil {
		return c.dopts.unaryInt(ctx, rpc, enc, in, out, c, finalInvoker)
	}
	return c.Conn.Invoke(ctx, rpc, enc, in, out)
}

// finalStreamer returns a Streamer which executes at the end in an interceptor chain.
func finalStreamer(ctx context.Context, rpc string, enc drpc.Encoding, cc *ClientConn) (drpc.Stream, error) {
	return cc.Conn.NewStream(ctx, rpc, enc)
}

func (c *ClientConn) NewStream(ctx context.Context, rpc string, enc drpc.Encoding) (drpc.Stream, error) {
	if c.dopts.streamInt != nil {
		return c.dopts.streamInt(ctx, rpc, enc, c, finalStreamer)
	}
	return c.Conn.NewStream(ctx, rpc, enc)
}

func (c *ClientConn) initInterceptors() {
	chainUnaryClientInterceptors(c)
	chainStreamClientInterceptors(c)
}

var _ drpc.Conn = (*ClientConn)(nil)

// chainUnaryClientInterceptors chains all unary client interceptors in the dialOptions into a single interceptor.
// The combined chained interceptor is stored in dopts.unaryInt. The interceptors are invoked in the order they were added.
//
// Example usage:
//
//	// Create a ClientConn and add interceptors
//	clientConn := &ClientConn{
//	    dopts: dialOptions{
//	        unaryInts: []UnaryClientInterceptor{loggingInterceptor, metricsInterceptor},
//	    },
//	}
//
//	// Chain the interceptors
//	chainUnaryClientInterceptors(clientConn)
//	// clientConn.dopts.unaryInt now contains the chained unary interceptor.
func chainUnaryClientInterceptors(cc *ClientConn) {
	switch n := len(cc.dopts.unaryInts); n {
	case 0:
		cc.dopts.unaryInt = nil
	case 1:
		cc.dopts.unaryInt = cc.dopts.unaryInts[0]
	default:
		cc.dopts.unaryInt = func(ctx context.Context, rpc string, enc drpc.Encoding, in, out drpc.Message, conn *ClientConn, invoker UnaryInvoker) error {
			chained := invoker
			for i := n - 1; i >= 0; i-- {
				next := chained
				interceptor := cc.dopts.unaryInts[i]
				chained = func(ctx context.Context, rpc string, enc drpc.Encoding, in, out drpc.Message, clientConn *ClientConn) error {
					return interceptor(ctx, rpc, enc, in, out, clientConn, next)
				}
			}
			return chained(ctx, rpc, enc, in, out, conn)
		}
	}
}

// chainStreamClientInterceptors chains all stream client interceptors in the dialOptions into a single interceptor.
// The combined chained stream interceptor is stored in dopts.streamInt. The interceptors are invoked in the order they were added.
//
// Example usage:
//
//	// Create a ClientConn and add interceptors
//	clientConn := &ClientConn{
//	    dopts: dialOptions{
//	        streamInts: []StreamClientInterceptor{loggingInterceptor, metricsInterceptor},
//	    },
//	}
//
//	// Chain the interceptors
//	chainStreamClientInterceptors(clientConn)
//	// clientConn.dopts.streamInt now contains the chained stream interceptor.
func chainStreamClientInterceptors(cc *ClientConn) {
	n := len(cc.dopts.streamInts)
	switch n {
	case 0:
		cc.dopts.streamInt = nil
	case 1:
		cc.dopts.streamInt = cc.dopts.streamInts[0]
	default:
		cc.dopts.streamInt = func(ctx context.Context, rpc string, enc drpc.Encoding, conn *ClientConn, streamer Streamer) (drpc.Stream, error) {
			chained := streamer
			for i := n - 1; i >= 0; i-- {
				next := chained
				interceptor := cc.dopts.streamInts[i]
				chained = func(ctx context.Context, rpc string, enc drpc.Encoding, clientConn *ClientConn) (drpc.Stream, error) {
					return interceptor(ctx, rpc, enc, clientConn, next)
				}
			}
			return chained(ctx, rpc, enc, conn)
		}
	}
}
