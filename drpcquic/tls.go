// Copyright (C) 2026 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcquic

import "crypto/tls"

// ensureALPN returns a clone of tlsConf with the drpcquic ALPN injected into
// NextProtos if none is set. QUIC requires a non-nil tls.Config with an
// application protocol; a nil input yields a config advertising only ALPN
// (suitable for a client verifying the server against system roots).
func ensureALPN(tlsConf *tls.Config) *tls.Config {
	if tlsConf == nil {
		tlsConf = &tls.Config{}
	} else {
		tlsConf = tlsConf.Clone()
	}
	if len(tlsConf.NextProtos) == 0 {
		tlsConf.NextProtos = []string{ALPN}
	}
	return tlsConf
}
