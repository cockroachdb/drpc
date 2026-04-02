// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcserver

import (
	"context"
	"crypto/tls"
	"net"
	"sync"
	"time"

	"github.com/zeebo/errs"
	"storj.io/drpc"
	"storj.io/drpc/drpccache"
	"storj.io/drpc/drpcctx"
	"storj.io/drpc/drpcmanager"
	"storj.io/drpc/drpcmetrics"
	"storj.io/drpc/drpcstats"
	"storj.io/drpc/drpcstream"
	"storj.io/drpc/internal/drpcopts"
)

// Options controls configuration settings for a server.
type Options struct {
	// Manager controls the options we pass to the managers this server creates.
	Manager drpcmanager.Options

	// Log is called when errors happen that can not be returned up, like
	// temporary network errors when accepting connections, or errors
	// handling individual clients. It is not called if nil.
	Log func(error)

	// CollectStats controls whether the server should collect stats on the
	// rpcs it serves.
	CollectStats bool

	// TLSConfig, if non-nil, is used to wrap the listener with tls.NewListener
	// in Serve(). The TLS handshake is performed explicitly in ServeOne before
	// processing requests.
	TLSConfig *tls.Config

	// TLSCipherRestrict, if non-nil, is called in ServeOne immediately after
	// a successful TLS handshake. It receives the net.Conn (which is a
	// *tls.Conn) and may inspect ConnectionState to enforce cipher suite
	// restrictions. If it returns a non-nil error the connection is rejected.
	TLSCipherRestrict func(conn net.Conn) error

	// Metrics holds optional metrics the server will populate.
	Metrics ServerMetrics
}

// ServerMetrics holds optional metrics that the server will populate during
// operation.
// Metrics are defined and registered by the caller (e.g. in CockroachDB) and
// passed in; this package never imports a metrics library.
type ServerMetrics struct {
	TLSHandshakeErrors drpcmetrics.Counter
}

// recordTLSHandshakeError increments the TLS handshake error counter.
func (s *Server) recordTLSHandshakeError() {
	s.opts.Metrics.TLSHandshakeErrors.Inc(1)
}

// Server is an implementation of drpc.Server to serve drpc connections.
type Server struct {
	opts    Options
	handler drpc.Handler

	mu    sync.Mutex
	stats map[string]*drpcstats.Stats
}

// New constructs a new Server.
func New(handler drpc.Handler) *Server {
	return NewWithOptions(handler, Options{})
}

// NewWithOptions constructs a new Server using the provided options to tune
// how the drpc connections are handled.
func NewWithOptions(handler drpc.Handler, opts Options) *Server {
	// Clone the TLS config so the server owns its copy and the caller cannot
	// mutate it after construction.
	if opts.TLSConfig != nil {
		opts.TLSConfig = opts.TLSConfig.Clone()
	}

	s := &Server{
		opts:    opts,
		handler: handler,
	}
	if s.opts.CollectStats {
		// TODO: (server): deprecate stats
		drpcopts.SetManagerStatsCB(&s.opts.Manager.Internal, s.getStats)
		s.stats = make(map[string]*drpcstats.Stats)
	}
	if s.opts.Metrics.TLSHandshakeErrors == nil {
		s.opts.Metrics.TLSHandshakeErrors = drpcmetrics.NoOpCounter{}
	}
	return s
}

// Stats returns the collected stats grouped by rpc.
func (s *Server) Stats() map[string]drpcstats.Stats {
	s.mu.Lock()
	defer s.mu.Unlock()

	stats := make(map[string]drpcstats.Stats, len(s.stats))
	for k, v := range s.stats {
		stats[k] = v.AtomicClone()
	}
	return stats
}

// getStats returns the drpcopts.Stats struct for the given rpc.
func (s *Server) getStats(rpc string) *drpcstats.Stats {
	s.mu.Lock()
	defer s.mu.Unlock()

	stats := s.stats[rpc]
	if stats == nil {
		stats = new(drpcstats.Stats)
		s.stats[rpc] = stats
	}
	return stats
}

// ServeOne serves a single set of rpcs on the provided transport.
func (s *Server) ServeOne(ctx context.Context, tr drpc.Transport) (err error) {
	// Check if the transport is a TLS connection
	if tlsConn, ok := tr.(*tls.Conn); ok {
		// Manually perform the TLS handshake to access peer certificate
		// information. In Go's TLS implementation, the handshake is normally
		// performed lazily on the first read/write operation. However, the
		// transport received by ServeOne hasn't performed any I/O yet, so
		// ConnectionState() would be empty. Only after the handshake completes
		// is ConnectionState populated with peer certificates and other
		// connection details that we need for authentication context.
		//
		// This explicit Handshake() call is safe and appropriate here. The
		// connection hasn't started processing requests yet, so we're not
		// interrupting any ongoing communication. Even if we didn't call it
		// explicitly, the first read/write operation would call it internally
		// anyway.
		err := tlsConn.HandshakeContext(ctx)
		if err != nil {
			s.recordTLSHandshakeError()
			return drpc.ConnectionError.New("server handshake [%q] failed: %w", tlsConn.RemoteAddr(), err)
		}
		if s.opts.TLSCipherRestrict != nil {
			if err := s.opts.TLSCipherRestrict(tlsConn); err != nil {
				s.recordTLSHandshakeError()
				return drpc.ConnectionError.New("server handshake [%q] failed: %w", tlsConn.RemoteAddr(), err)
			}
		}
		state := tlsConn.ConnectionState()
		if len(state.PeerCertificates) > 0 {
			ctx = drpcctx.WithPeerConnectionInfo(
				ctx, drpcctx.PeerConnectionInfo{Certificates: state.PeerCertificates})
		}
	}

	man := drpcmanager.NewWithOptions(tr, s.opts.Manager)
	defer func() { err = errs.Combine(err, man.Close()) }()

	cache := drpccache.New()
	defer cache.Clear()

	ctx = drpccache.WithContext(ctx, cache)

	for {
		stream, rpc, err := man.NewServerStream(ctx)
		if err != nil {
			return errs.Wrap(err)
		}
		if err := s.handleRPC(stream, rpc); err != nil {
			return errs.Wrap(err)
		}
	}
}

var temporarySleep = 500 * time.Millisecond

// Serve listens for connections on the listener and serves the drpc request
// on new connections.
func (s *Server) Serve(ctx context.Context, lis net.Listener) (err error) {
	if s.opts.TLSConfig != nil {
		lis = tls.NewListener(lis, s.opts.TLSConfig)
	}

	tracker := drpcctx.NewTracker(ctx)
	defer tracker.Wait()
	defer tracker.Cancel()

	tracker.Run(func(ctx context.Context) {
		<-ctx.Done()
		_ = lis.Close()
	})

	for {
		conn, err := lis.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}

			if isTemporary(err) {
				if s.opts.Log != nil {
					s.opts.Log(err)
				}

				t := time.NewTimer(temporarySleep)
				select {
				case <-t.C:
				case <-ctx.Done():
					t.Stop()
					return nil
				}

				continue
			}

			return errs.Wrap(err)
		}

		// TODO(jeff): connection limits?
		tracker.Run(func(ctx context.Context) {
			err := s.ServeOne(ctx, conn)
			if err != nil && s.opts.Log != nil {
				s.opts.Log(err)
			}
		})
	}
}

// handleRPC handles the rpc that has been requested by the stream.
func (s *Server) handleRPC(stream *drpcstream.Stream, rpc string) (err error) {
	err = s.handler.HandleRPC(stream, rpc)
	if err != nil {
		return errs.Wrap(stream.SendError(err))
	}
	return errs.Wrap(stream.CloseSend())
}
