// Copyright (C) 2026 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcquic

import (
	"context"
	"crypto/tls"
	"net"

	"github.com/quic-go/quic-go"
)

// Dial establishes a QUIC connection to addr and returns it as a
// drpc.MultiplexedTransport. tlsConf may be nil (the client then verifies the
// server against the system roots); the drpcquic ALPN is injected if absent.
func Dial(ctx context.Context, addr string, tlsConf *tls.Config, opts Options) (*Transport, error) {
	conn, err := quic.DialAddr(ctx, addr, ensureALPN(tlsConf), opts.quicConfig())
	if err != nil {
		return nil, mapQUICError(err)
	}
	return newTransport(conn), nil
}

// Listen creates a QUIC listener for drpc on addr. tlsConf must provide a
// certificate; the drpcquic ALPN is injected if NextProtos is empty.
func Listen(addr string, tlsConf *tls.Config, opts Options) (*Listener, error) {
	l, err := quic.ListenAddr(addr, ensureALPN(tlsConf), opts.quicConfig())
	if err != nil {
		return nil, err
	}
	return &Listener{lis: l}, nil
}

// ListenPacket creates a QUIC listener for drpc on a caller-provided UDP socket.
// Use this when the caller wants to own socket creation (e.g. for address /
// advertise-address handling). tlsConf must provide a certificate; the drpcquic
// ALPN is injected if NextProtos is empty.
func ListenPacket(conn net.PacketConn, tlsConf *tls.Config, opts Options) (*Listener, error) {
	l, err := quic.Listen(conn, ensureALPN(tlsConf), opts.quicConfig())
	if err != nil {
		return nil, err
	}
	return &Listener{lis: l}, nil
}
