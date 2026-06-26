// Copyright (C) 2026 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcmanager

import (
	"context"
	"time"

	grpcmetadata "google.golang.org/grpc/metadata"

	"storj.io/drpc"
	"storj.io/drpc/drpcmetadata"
	"storj.io/drpc/drpcstream"
	"storj.io/drpc/drpcwire"
	"storj.io/drpc/internal/drpcopts"
)

// QUICManager manages a drpc.MultiplexedTransport (e.g. a QUIC connection),
// mapping each drpc logical stream onto its own native stream. Unlike Manager,
// there is no shared reader, no semaphore, no streamBuffer, and no pkts/pdone
// signaling: each stream owns its underlying stream and reads it directly via
// drpcstream.NewForReader.
type QUICManager struct {
	mt   drpc.MultiplexedTransport
	opts Options
}

// NewQUIC returns a new QUICManager over the multiplexed transport.
func NewQUIC(mt drpc.MultiplexedTransport, opts Options) *QUICManager {
	return &QUICManager{mt: mt, opts: opts}
}

// Close closes the multiplexed transport, which closes all of its streams.
func (m *QUICManager) Close() error { return m.mt.Close() }

// Closed returns a channel closed when the multiplexed transport is closed.
func (m *QUICManager) Closed() <-chan struct{} { return m.mt.Closed() }

// Unblocked never blocks in multiplexed mode: there is no single-stream
// semaphore, so a new stream can always be created. It returns an already
// closed channel to satisfy the manager interface used by drpcconn.Conn.
func (m *QUICManager) Unblocked() <-chan struct{} { return closedCh }

// streamOpts builds per-stream options for a stream on transport tr. It copies
// m.opts.Stream so concurrent NewClientStream calls do not race on shared
// option state. The fin channel is deliberately left unset: in multiplexed mode
// the stream is not coordinated through a manager semaphore, and checkFinished
// nil-guards the fin channel.
func (m *QUICManager) streamOpts(kind drpc.StreamKind, rpc string, tr drpc.Transport) drpcstream.Options {
	opts := m.opts.Stream
	drpcopts.SetStreamKind(&opts.Internal, kind)
	drpcopts.SetStreamRPC(&opts.Internal, rpc)
	drpcopts.SetStreamTransport(&opts.Internal, tr)
	if cb := drpcopts.GetManagerStatsCB(&m.opts.Internal); cb != nil {
		drpcopts.SetStreamStats(&opts.Internal, cb(rpc))
	}
	return opts
}

// NewClientStream opens a new outbound stream and wraps it in a drpc stream with
// its own Reader/Writer and read loop. The caller (drpcconn) writes the invoke
// sequence on the returned stream.
func (m *QUICManager) NewClientStream(ctx context.Context, rpc string) (*drpcstream.Stream, error) {
	tr, err := m.mt.OpenStream(ctx)
	if err != nil {
		return nil, err
	}
	rd := drpcwire.NewReaderWithOptions(tr, m.opts.Reader)
	wr := drpcwire.NewWriter(tr, m.opts.WriterBufferSize)
	// sid MUST be 1: the reader starts expecting ID{Stream:1, Message:1}.
	return drpcstream.NewForReader(ctx, 1, tr, rd, wr, m.streamOpts(drpc.StreamKindClient, rpc, tr)), nil
}

// AcceptTransport accepts the next inbound stream and returns its raw transport
// WITHOUT reading the invoke. Hand the transport to ServerStream from a
// per-stream goroutine so that reading one stream's invoke never blocks
// accepting (or serving) the next stream on the same connection. Doing the
// invoke read here (as a combined accept+read) would serialize the whole
// connection behind whichever stream is slowest to send its invoke — exactly
// the head-of-line blocking running over QUIC is meant to avoid.
func (m *QUICManager) AcceptTransport(ctx context.Context) (drpc.Transport, error) {
	return m.mt.AcceptStream(ctx)
}

// ServerStream reads the invoke (and any preceding metadata) packets off an
// already-accepted transport tr, then hands the SAME reader to the stream's read
// loop so no buffered bytes are lost and no second reader races the first. On any
// error it closes tr, so a failed or slow stream leaks nothing and never affects
// other streams on the connection.
func (m *QUICManager) ServerStream(ctx context.Context, tr drpc.Transport) (stream *drpcstream.Stream, rpc string, err error) {
	defer func() {
		if err != nil {
			_ = tr.Close() // don't leak the stream on a failed parse
		}
	}()

	// Optionally bound how long we wait for the client's invoke.
	if to := m.opts.InactivityTimeout; to > 0 {
		if d, ok := tr.(interface{ SetReadDeadline(time.Time) error }); ok {
			_ = d.SetReadDeadline(time.Now().Add(to))
			defer func() { _ = d.SetReadDeadline(time.Time{}) }()
		}
	}

	rd := drpcwire.NewReaderWithOptions(tr, m.opts.Reader)

	var meta map[string]string
	var metaID uint64
	for {
		pkt, perr := rd.ReadPacketUsing(nil)
		if perr != nil {
			return nil, "", perr
		}
		switch pkt.Kind {
		case drpcwire.KindInvokeMetadata:
			meta, err = drpcmetadata.Decode(pkt.Data)
			if err != nil {
				return nil, "", err
			}
			metaID = pkt.ID.Stream

		case drpcwire.KindInvoke:
			rpc = string(pkt.Data)
			if metaID == pkt.ID.Stream {
				if m.opts.GRPCMetadataCompatMode {
					grpcMeta := make(map[string][]string, len(meta))
					for k, v := range meta {
						grpcMeta[k] = []string{v}
					}
					ctx = grpcmetadata.NewIncomingContext(ctx, grpcMeta)
				} else {
					ctx = drpcmetadata.NewIncomingContext(ctx, meta)
				}
			}
			wr := drpcwire.NewWriter(tr, m.opts.WriterBufferSize)
			stream = drpcstream.NewForReader(ctx, pkt.ID.Stream, tr, rd, wr,
				m.streamOpts(drpc.StreamKindServer, rpc, tr))
			return stream, rpc, nil

		default:
			return nil, "", drpc.ProtocolError.New("expected invoke, got %s", pkt.Kind)
		}
	}
}

// NewServerStream accepts a new inbound stream and reads its invoke. It is
// AcceptTransport followed by ServerStream. Prefer the split form when serving
// many concurrent streams (AcceptTransport in the accept loop, ServerStream in a
// per-stream goroutine) so the invoke read does not serialize accepts; this
// combined form remains for single-stream callers and tests.
func (m *QUICManager) NewServerStream(ctx context.Context) (stream *drpcstream.Stream, rpc string, err error) {
	tr, err := m.mt.AcceptStream(ctx)
	if err != nil {
		return nil, "", err
	}
	return m.ServerStream(ctx, tr)
}
