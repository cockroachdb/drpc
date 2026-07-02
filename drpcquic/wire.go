// Copyright (C) 2024 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcquic

import (
	"crypto/tls"
	"net"

	"github.com/quic-go/quic-go"
)

// ALPN is the application protocol negotiated for DRPC-over-QUIC. QUIC mandates
// an ALPN protocol; Dial and Listen force this value on both ends.
const ALPN = "drpc-quic"

// defaultMaxMessageSize bounds the size of a single message read off a QUIC
// stream. Because each message is written as a single frame, this is the one
// remaining reason to cap a frame (defense against a malicious peer); QUIC
// itself handles packetization and flow control.
const defaultMaxMessageSize = 4 << 20

// quicCancelCode is the QUIC stream error code used when abruptly canceling a
// stream's read or write side.
const quicCancelCode quic.StreamErrorCode = 0

// quicConnCloseCode is the QUIC application error code used when closing a
// connection.
const quicConnCloseCode quic.ApplicationErrorCode = 0

// ensureALPN returns a clone of tlsConf whose ALPN protocol list is exactly the
// DRPC application protocol. Both client and server must agree on the ALPN, so
// we force ours (overriding any other protocols, e.g. "h2", that the base config
// may list) so the handshake succeeds regardless of how the caller built it.
func ensureALPN(tlsConf *tls.Config) *tls.Config {
	if tlsConf == nil {
		tlsConf = &tls.Config{}
	} else {
		tlsConf = tlsConf.Clone()
	}
	tlsConf.NextProtos = []string{ALPN}
	// Some configs (e.g. CockroachDB's server config) complete the handshake via
	// a config returned from GetConfigForClient for cert hot-reloading, which
	// would otherwise drop our NextProtos. Wrap it so the dynamically-selected
	// config also advertises our ALPN protocol.
	if base := tlsConf.GetConfigForClient; base != nil {
		tlsConf.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			cfg, err := base(chi)
			if err != nil || cfg == nil {
				return cfg, err
			}
			cfg = cfg.Clone()
			cfg.NextProtos = []string{ALPN}
			return cfg, nil
		}
	}
	return tlsConf
}

// Listen starts a QUIC listener for DRPC on addr. tlsConf must carry a server
// certificate (QUIC is secure-only); the DRPC ALPN is forced automatically.
func Listen(addr string, tlsConf *tls.Config) (*quic.Listener, error) {
	// 1-RTT quic.ListenAddr, not the 0-RTT "early" variant: 0-RTT early data is
	// replayable and is deliberately deferred (see the note in Dial).
	return quic.ListenAddr(addr, ensureALPN(tlsConf), nil)
}

// ListenPacket is like Listen but over a caller-owned UDP socket, for callers
// that want to own socket creation (e.g. sharing a port). tlsConf must carry a
// server certificate; the DRPC ALPN is forced automatically.
func ListenPacket(conn net.PacketConn, tlsConf *tls.Config) (*quic.Listener, error) {
	return quic.Listen(conn, ensureALPN(tlsConf), nil)
}
