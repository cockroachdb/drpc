// Copyright (C) 2023 Elara Musayelyan
// Copyright (C) 2025 Cockroach Labs
// See LICENSE for copying information.

package drpcyamux

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"

	"github.com/hashicorp/yamux"
	"storj.io/drpc"
	"storj.io/drpc/drpcctx"
	"storj.io/drpc/drpcserver"
)

// Server is a DRPC server that handles multiplexed streams
type Server struct {
	srv *drpcserver.Server
}

// NewServer creates a new multiplexing DRPC server with default options
func NewServer(handler drpc.Handler) *Server {
	return &Server{srv: drpcserver.New(handler)}
}

// NewServerWithOptions creates a new multiplexing DRPC server with custom options
func NewServerWithOptions(handler drpc.Handler, opts drpcserver.Options) *Server {
	return &Server{srv: drpcserver.NewWithOptions(handler, opts)}
}

// Serve listens on the given listener and handles all multiplexed streams.
// It blocks until the context is canceled or an unrecoverable error occurs.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	var wg sync.WaitGroup
	defer wg.Wait()

	// Context for coordinating shutdown
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	for {
		conn, err := ln.Accept()
		if err != nil {
			// Check if we're shutting down
			select {
			case <-ctx.Done():
				return nil
			default:
			}

			// If listener was closed, treat it as shutdown
			var opErr *net.OpError
			if errors.As(err, &opErr) && opErr.Op == "accept" {
				return nil
			}

			return err
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleConn(ctx, conn)
		}()
	}
}

// handleConn processes a single connection with multiplexing
func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	if tlsConn, ok := conn.(*tls.Conn); ok {
		err := tlsConn.Handshake()
		if err != nil {
			return
		}
		state := tlsConn.ConnectionState()
		if len(state.PeerCertificates) > 0 {
			ctx = drpcctx.WithPeerConnectionInfo(
				ctx, drpcctx.PeerConnectionInfo{Certificates: state.PeerCertificates})
		}
	}

	sess, err := yamux.Server(conn, nil)
	if err != nil {
		return
	}
	defer sess.Close()

	s.handleSession(ctx, sess)
}

// handleSession accepts and serves streams from a yamux session
func (s *Server) handleSession(ctx context.Context, sess *yamux.Session) {
	var wg sync.WaitGroup
	defer wg.Wait()

	// Close session when context is cancelled
	done := make(chan struct{})
	defer close(done)

	go func() {
		select {
		case <-ctx.Done():
			sess.Close()
		case <-done:
		}
	}()

	for {
		stream, err := sess.Accept()
		if err != nil {
			// Any error from Accept means the session is done
			// Common errors: io.EOF (graceful close), session closed, etc.
			return
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			s.srv.ServeOne(ctx, stream)
		}()
	}
}
