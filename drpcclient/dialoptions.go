package drpcclient

// dialOptions configure a NewClientConnWithOptions call. dialOptions are set by the DialOption
// values passed to NewClientConnWithOptions.
type dialOptions struct {
	unaryInt  UnaryClientInterceptor
	streamInt StreamClientInterceptor

	unaryInts  []UnaryClientInterceptor
	streamInts []StreamClientInterceptor
}

// DialOption configures how we set up the client connection.
type DialOption func(options *dialOptions)

func defaultDialOptions() dialOptions {
	return dialOptions{}
}

// WithChainUnaryInterceptor returns a DialOption that adds one or more unary RPC interceptors,
// chaining. Last interceptor is the innermost which eventually invokes the UnaryInvoker.
func WithChainUnaryInterceptor(ints ...UnaryClientInterceptor) DialOption {
	return func(opt *dialOptions) {
		opt.unaryInts = append(opt.unaryInts, ints...)
	}
}

// WithChainStreamInterceptor returns a DialOption that adds one or more stream RPC interceptors,
// chaining. Last interceptor is the innermost which eventually invokes the Streamer.
func WithChainStreamInterceptor(ints ...StreamClientInterceptor) DialOption {
	return func(opt *dialOptions) {
		opt.streamInts = append(opt.streamInts, ints...)
	}
}
