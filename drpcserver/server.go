// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcserver

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/zeebo/errs"

	"storj.io/drpc"
	"storj.io/drpc/drpccache"
	"storj.io/drpc/drpcctx"
	"storj.io/drpc/drpcmanager"
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

	serverInt  ServerInterceptor
	serverInts []ServerInterceptor
}

// A ServerOption sets options such as server interceptors.
type ServerOption func(options *Options)

// WithChainServerInterceptor creates a ServerOption that chains multiple server interceptors,
// with the first being the outermost wrapper and the last being the innermost.
func WithChainServerInterceptor(ints ...ServerInterceptor) ServerOption {
	return func(opt *Options) {
		opt.serverInts = append(opt.serverInts, ints...)
	}
}

// chainServerInterceptors chains all server interceptors in the Options into a single interceptor.
// The combined chained interceptor is stored in opts.serverInt. The interceptors are invoked in the order they were added.
//
// Example usage:
//
//	Interceptors are typically added using WithChainServerInterceptor when creating the server.
//	The NewWithOptions function calls chainServerInterceptors internally to process these.
//	server := drpcserver.NewWithOptions(
//	    drpcHandler,
//	    drpcserver.Options{}, // base server options
//	    drpcserver.WithChainServerInterceptor(loggingInterceptor, metricsInterceptor),
//	)
//
//	// Chain the interceptors
//	chainServerInterceptors(server)
//	// server.opts.serverInt now contains the chained server interceptors.
func chainServerInterceptors(s *Server) {
	switch n := len(s.opts.serverInts); n {
	case 0:
		s.opts.serverInt = nil
	case 1:
		s.opts.serverInt = s.opts.serverInts[0]
	default:
		s.opts.serverInt = func(
			ctx context.Context,
			rpc string,
			stream drpc.Stream,
			handler drpc.Handler,
		) error {
			chained := handler
			for i := n - 1; i >= 0; i-- {
				next := chained
				interceptor := s.opts.serverInts[i]
				chainedFn := func(
					stream drpc.Stream,
					rpc string,
				) error {
					return interceptor(ctx, rpc, stream, next)
				}
				chained = HandlerFunc(chainedFn)
			}
			return chained.HandleRPC(stream, rpc)
		}
	}
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
func NewWithOptions(handler drpc.Handler, opts Options, sopts ...ServerOption) *Server {
	s := &Server{
		opts:    opts,
		handler: handler,
	}

	if s.opts.CollectStats {
		drpcopts.SetManagerStatsCB(&s.opts.Manager.Internal, s.getStats)
		s.stats = make(map[string]*drpcstats.Stats)
	}
	for _, opt := range sopts {
		opt(&s.opts)
	}
	chainServerInterceptors(s)

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
		if err := s.handleRPC(ctx, stream, rpc); err != nil {
			return errs.Wrap(err)
		}
	}
}

var temporarySleep = 500 * time.Millisecond

// Serve listens for connections on the listener and serves the drpc request
// on new connections.
func (s *Server) Serve(ctx context.Context, lis net.Listener) (err error) {
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
func (s *Server) handleRPC(ctx context.Context, stream *drpcstream.Stream, rpc string) (err error) {
	var processingErr error
	if s.opts.serverInt != nil {
		processingErr = s.opts.serverInt(ctx, rpc, stream, s.handler)
	} else {
		processingErr = s.handler.HandleRPC(stream, rpc)
	}

	if processingErr != nil {
		return errs.Wrap(stream.SendError(processingErr))
	}
	return errs.Wrap(stream.CloseSend())
}
