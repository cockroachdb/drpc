// Copyright (C) 2026 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcquic

import (
	"context"
	"crypto/tls"
	"testing"

	"github.com/stretchr/testify/require"

	"storj.io/drpc"
	"storj.io/drpc/drpcserver"
)

// startServer starts a drpcquic server on a loopback address serving handler h.
// It returns the dial address, a matching client tls.Config, and a stop func.
func startServer(t *testing.T, h drpc.Handler, opts Options) (addr string, clientTLS *tls.Config, stop func()) {
	t.Helper()
	serverTLS, clientTLS := testTLS(t)
	ln, err := Listen("127.0.0.1:0", serverTLS, opts)
	require.NoError(t, err)

	srv := drpcserver.New(h)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = Serve(ctx, ln, srv, opts); close(done) }()

	return ln.Addr().String(), clientTLS, func() {
		cancel()
		_ = ln.Close()
		<-done
	}
}
