package drpcinterceptors

import (
	"context"
	"storj.io/drpc"
)

type UnaryClientInterceptor func(ctx context.Context, rpc string, in, out drpc.Message, cc drpc.Conn, enc drpc.Encoding, next UnaryInvoker) error

type UnaryInvoker func(ctx context.Context, rpc string, in, out drpc.Message, enc drpc.Encoding) error

// Streamer is a function that opens a new DRPC stream.
type Streamer func(ctx context.Context, method string) (drpc.Stream, error)

// StreamClientInterceptor is the DRPC equivalent of a gRPC stream client interceptor.
type StreamClientInterceptor func(ctx context.Context, method string, conn drpc.Conn, streamer Streamer) (drpc.Stream, error)

// this is same as ClientConnOptions
type DialOptions struct {
	UnaryInt   UnaryClientInterceptor
	unaryInts  []UnaryClientInterceptor
	StreamInt  StreamClientInterceptor
	streamInts []StreamClientInterceptor
}

type DialOption func(options *DialOptions)

func WithChainUnaryInterceptor(ints ...UnaryClientInterceptor) DialOption {
	return func(opt *DialOptions) {
		opt.unaryInts = append(opt.unaryInts, ints...)
	}
}

func WithChainStreamInterceptor(ints ...StreamClientInterceptor) DialOption {
	return func(opt *DialOptions) {
		opt.streamInts = append(opt.streamInts, ints...)
	}
}

func NewDialOptions(opts []DialOption) *DialOptions {
	options := &DialOptions{}
	for _, opt := range opts {
		opt(options)
	}
	return options
}

// ChainUnaryClientInterceptors chains all unary client interceptors in the DialOptions into a single interceptor.
//
// This method inspects the slice of unary client interceptors (`d.unaryInts`) and combines them into one interceptor,
// assigning the result to `d.UnaryInt`. If there are no interceptors, `d.UnaryInt` is set to nil. If there is only one,
// it is used directly. If there are multiple, they are chained so that each interceptor can process the request and
// pass control to the next, ending with the original invoker.
//
// The interceptors are invoked in the order they were added.
//
// Example usage:
//
//	opts := &drpcinterceptors.DialOptions{}
//	drpcinterceptors.WithChainUnaryInterceptor(loggingInterceptor, authInterceptor)(opts)
//	opts.ChainUnaryClientInterceptors()
//	// opts.UnaryInt now contains the chained interceptor.
//
// Side effects:
//   - Sets d.UnaryInt to the chained interceptor or nil.
func (d *DialOptions) ChainUnaryClientInterceptors() {
	switch n := len(d.unaryInts); n {
	case 0:
		d.UnaryInt = nil
	case 1:
		d.UnaryInt = d.unaryInts[0]
	default:
		d.UnaryInt = func(ctx context.Context, method string, in, out drpc.Message, conn drpc.Conn, enc drpc.Encoding, invoker UnaryInvoker) error {
			chained := invoker
			for i := n - 1; i >= 0; i-- {
				next := chained
				interceptor := d.unaryInts[i]
				chained = func(ctx context.Context, method string, in, out drpc.Message, enc drpc.Encoding) error {
					return interceptor(ctx, method, in, out, conn, enc, next)
				}
			}
			return chained(ctx, method, in, out, enc)
		}
	}
}

// ChainStreamClientInterceptors chains all stream client interceptors in the DialOptions into a single interceptor.
//
// This method examines the slice of stream client interceptors (`d.streamInts`) and combines them into one interceptor,
// assigning the result to `d.StreamInt`. If there are no interceptors, `d.StreamInt` is set to nil. If there is only one,
// it is used directly. If there are multiple, they are chained so that each interceptor can process the stream request
// and pass control to the next, ending with the original streamer.
//
// The interceptors are invoked in the order they were added.
//
// Example usage:
//
//	opts := &drpcinterceptors.DialOptions{}
//	drpcinterceptors.WithChainStreamInterceptor(loggingInterceptor, metricsInterceptor)(opts)
//	opts.ChainStreamClientInterceptors()
//	// opts.StreamInt now contains the chained stream interceptor.
//
// Side effects:
//   - Sets d.StreamInt to the chained interceptor or nil.
func (d *DialOptions) ChainStreamClientInterceptors() {
	n := len(d.streamInts)
	switch n {
	case 0:
		d.StreamInt = nil
	case 1:
		d.StreamInt = d.streamInts[0]
	default:
		d.StreamInt = func(ctx context.Context, method string, conn drpc.Conn, streamer Streamer) (drpc.Stream, error) {
			chained := streamer
			for i := n - 1; i >= 0; i-- {
				next := chained
				interceptor := d.streamInts[i]
				chained = func(ctx context.Context, method string) (drpc.Stream, error) {
					return interceptor(ctx, method, conn, next)
				}
			}
			return chained(ctx, method)
		}
	}
}
