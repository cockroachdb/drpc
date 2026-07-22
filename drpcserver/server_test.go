// Copyright (C) 2022 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcserver

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/zeebo/assert"

	"storj.io/drpc/drpctest"
)

func init() { temporarySleep = 0 }

func TestServerTemporarySleep(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	calls := 0
	l := listener(func() (net.Conn, error) {
		calls++
		switch calls {
		case 1:
		case 2:
			ctx.Cancel()
		default:
			panic("spinning on temporary error")
		}

		return nil, new(temporaryError)
	})

	assert.NoError(t, New(nil).Serve(ctx, l))
}

// TestServeOneTLSHandshakeTimeout verifies that TLSHandshakeTimeout bounds the
// handshake ServeOne drives. When the timeout is unset the handshake is bounded
// only by the parent context, which must still interrupt a stalled handshake.
func TestServeOneTLSHandshakeTimeout(t *testing.T) {
	t.Run("times out a stalled handshake", func(t *testing.T) {
		ctx := drpctest.NewTracker(t)
		defer ctx.Close()

		// The client end never writes, so the server handshake blocks reading
		// the ClientHello until the timeout cancels the handshake context.
		srv, clt := net.Pipe()
		defer func() { _ = srv.Close() }()
		defer func() { _ = clt.Close() }()

		s := NewWithOptions(nil, Options{TLSHandshakeTimeout: 50 * time.Millisecond})

		done := make(chan error, 1)
		ctx.Run(func(ctx context.Context) {
			done <- s.ServeOne(ctx, tls.Server(srv, &tls.Config{}))
		})

		select {
		case err := <-done:
			assert.Error(t, err) // handshake timeout fired
		case <-time.After(30 * time.Second):
			t.Fatal("ServeOne did not return; the handshake was not bounded")
		}
	})

	t.Run("unset timeout still honors parent context", func(t *testing.T) {
		ctx := drpctest.NewTracker(t)
		defer ctx.Close()

		srv, clt := net.Pipe()
		defer func() { _ = srv.Close() }()
		defer func() { _ = clt.Close() }()

		s := NewWithOptions(nil, Options{}) // zero timeout: bounded only by ctx

		done := make(chan error, 1)
		ctx.Run(func(ctx context.Context) {
			done <- s.ServeOne(ctx, tls.Server(srv, &tls.Config{}))
		})

		// With no handshake timeout the stalled handshake blocks until the
		// parent context is canceled, at which point HandshakeContext closes the
		// connection and returns.
		ctx.Cancel()

		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("ServeOne did not return after context cancellation")
		}
	})
}

type listener func() (net.Conn, error)

func (l listener) Accept() (net.Conn, error) { return l() }
func (l listener) Close() error              { return nil }
func (l listener) Addr() net.Addr            { return nil }

type temporaryError struct{}

func (temporaryError) Error() string   { return "temporary error" }
func (temporaryError) Timeout() bool   { return false }
func (temporaryError) Temporary() bool { return true }
