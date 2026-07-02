// Copyright (C) 2024 Storj Labs, Inc.
// See LICENSE for copying information.

// Package drpcquic adapts DRPC onto QUIC: each DRPC stream maps 1:1 to its own
// native QUIC stream, so QUIC's independent streams provide the multiplexing
// (eliminating the transport-layer head-of-line blocking inherent to
// multiplexing many logical streams over a single byte transport). DRPC's
// framing, encoding, error and metadata helpers are reused per QUIC stream;
// DRPC's own multiplexing manager (drpcmanager) is bypassed entirely — a
// QuicConn implements drpc.Conn and a QuicStream implements drpc.Stream
// directly over quic-go, with no drpc.Transport middleman and no per-RPC
// reader/watcher goroutines.
package drpcquic
