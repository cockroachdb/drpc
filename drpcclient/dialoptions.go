package drpcclient

import (
	"context"
	"crypto/tls"
	"math"
	"net"

	"storj.io/drpc"
	"storj.io/drpc/drpcconn"
	"storj.io/drpc/drpcmanager"
	"storj.io/drpc/drpcmetrics"
	"storj.io/drpc/drpcstream"
	"storj.io/drpc/drpcwire"
)

// dialOptions configure a NewClientConnWithOptions call. dialOptions are set by the DialOption
// values passed to NewClientConnWithOptions.
type dialOptions struct {
	// Unary and stream interceptor chain.
	unaryInt  UnaryClientInterceptor
	streamInt StreamClientInterceptor

	// Unary and stream interceptors to be chained.
	unaryInts  []UnaryClientInterceptor
	streamInts []StreamClientInterceptor

	// RPC metadata to be added to each RPC call.
	perRPCMetadata map[string]string

	// dialer is an optional custom dialer function to use instead of default net.Dialer.
	dialer func(context.Context, string) (net.Conn, error)
	// tlsConfig is an optional TLS configuration for secure connections.
	tlsConfig *tls.Config

	// metrics holds optional metrics the conn will populate. No metrics are
	// recorded if this is nil. When shouldRecord is set, metrics are recorded
	// only when shouldRecord returns true.
	metrics      *drpcmetrics.ClientMetrics
	shouldRecord func() bool
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

func WithPerRPCMetadata(metadata map[string]string) DialOption {
	return func(opt *dialOptions) {
		opt.perRPCMetadata = metadata
	}
}

// WithTLSConfig returns a DialOption that sets the TLS configuration for
// secure connections.
func WithTLSConfig(tlsConfig *tls.Config) DialOption {
	return func(o *dialOptions) {
		o.tlsConfig = tlsConfig
	}
}

// WithMetrics returns a DialOption that sets client metrics to be populated
// during connection operation.
func WithMetrics(metrics *drpcmetrics.ClientMetrics) DialOption {
	return func(o *dialOptions) {
		o.metrics = metrics
	}
}

// WithShouldRecordFunc returns a DialOption that sets a function to control
// whether metrics are recorded. If the function returns false, no metrics
// are collected.
func WithShouldRecordFunc(shouldRecord func() bool) DialOption {
	return func(o *dialOptions) {
		o.shouldRecord = shouldRecord
	}
}

// WithContextDialer returns a DialOption that sets a custom dialer function
// to be used instead of the default net.Dialer.
func WithContextDialer(dialer func(context.Context, string) (net.Conn, error)) DialOption {
	return func(o *dialOptions) {
		o.dialer = dialer
	}
}

func DialContext(
	ctx context.Context, address string, opts ...DialOption,
) (conn *drpcconn.Conn, err error) {
	defer func() { err = drpc.ToRPCErr(err) }()

	var options dialOptions
	for _, opt := range opts {
		opt(&options)
	}

	var netConn net.Conn
	netConn, err = func() (net.Conn, error) {
		if options.dialer != nil {
			return options.dialer(ctx, address)
		}
		dialer := &net.Dialer{}
		return dialer.DialContext(ctx, "tcp", address)
	}()
	// gRPC classifies connection failures as Unavailable. Connection
	// errors include failures during TCP dialing as well as the TLS
	// handshake. For backward compatibility, we mirror gRPC's behavior
	// and return the same status codes to clients.
	if err != nil {
		return nil, drpc.ConnectionError.New("error while dialing target [%s]: %w", address, err)
	}

	if options.tlsConfig != nil {
		// Create a copy of the TLS configuration to prevent altering the
		// original copy.
		tlsConfig := options.tlsConfig.Clone()
		// Set the ServerName for TLS verification.
		var sn string
		sn, _, err = net.SplitHostPort(address)
		if err != nil {
			return nil, drpc.InternalError.New("invalid address [%s]: %w", address, err)
		}
		tlsConfig.ServerName = sn
		netConn = tls.Client(netConn, tlsConfig)

		err = netConn.(*tls.Conn).HandshakeContext(ctx)
		if err != nil {
			return nil, drpc.ConnectionError.New("client handshake [%q] failed: %w", address, err)
		}
	}

	if options.metrics == nil {
		options.metrics = &drpcmetrics.ClientMetrics{}
	}
	return drpcconn.NewWithOptions(netConn, drpcconn.Options{
		Manager: drpcmanager.Options{
			Reader: drpcwire.ReaderOptions{
				MaximumBufferSize: math.MaxInt,
			},
			Stream: drpcstream.Options{
				MaximumBufferSize: 0, // unlimited
			},
		},
		ShouldRecord: options.shouldRecord,
		Metrics:      *options.metrics,
	}), nil
}
