// Copyright (C) 2024 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcquic

import (
	"context"
	"io"
	"sync"

	"github.com/quic-go/quic-go"
	"github.com/zeebo/errs"
	grpcmetadata "google.golang.org/grpc/metadata"
	"storj.io/drpc"
	"storj.io/drpc/drpcctx"
	"storj.io/drpc/drpcenc"
	"storj.io/drpc/drpcmetadata"
	"storj.io/drpc/drpcwire"
)

// QuicStream adapts a single bidirectional QUIC stream to the drpc.Stream
// interface. It is used on both the client and server sides.
type QuicStream struct {
	stream *quic.Stream
	reader *drpcwire.Reader
	kind   drpc.StreamKind

	ctx context.Context
	// cancel cancels ctx; only set on the client, where ctx is independent of
	// the QUIC stream (see newClientStream). On the server, ctx is the QUIC
	// stream's own context and cancellation is native.
	cancel context.CancelFunc

	// stop deregisters the client-side context.AfterFunc watcher, if any.
	stop func() bool

	// grpcMetadataCompat, on the server, decodes incoming invoke metadata as
	// gRPC metadata in the context (mirrors drpcmanager's GRPCMetadataCompatMode).
	grpcMetadataCompat bool

	writeMu  sync.Mutex
	sendID   uint64 // next outgoing packet message id; the reader requires these to be monotonic and >= 1
	sendDone bool
	closed   bool

	recvErr error
}

var _ drpc.Stream = (*QuicStream)(nil)

// newReader builds the per-stream frame reader. maxMsg <= 0 uses the default cap.
func newReader(stream *quic.Stream, maxMsg int) *drpcwire.Reader {
	if maxMsg <= 0 {
		maxMsg = defaultMaxMessageSize
	}
	return drpcwire.NewReaderWithOptions(stream, drpcwire.ReaderOptions{
		MaximumBufferSize: maxMsg,
	})
}

// newServerStream wraps an accepted QUIC stream for server-side use. Its context
// is the QUIC stream's own context (which quic-go cancels natively when the peer
// resets the stream or when we close it — so a handler observes client
// cancellation via Context().Done() with no extra goroutine), enriched with the
// peer's TLS connection info so handlers can authenticate the remote.
func newServerStream(
	stream *quic.Stream, peerInfo drpcctx.PeerConnectionInfo, maxMsg int, grpcCompat bool,
) *QuicStream {
	return &QuicStream{
		stream:             stream,
		reader:             newReader(stream, maxMsg),
		kind:               drpc.StreamKindServer,
		ctx:                drpcctx.WithPeerConnectionInfo(stream.Context(), peerInfo),
		grpcMetadataCompat: grpcCompat,
	}
}

// newClientStream wraps a freshly opened QUIC stream for client-side use. Its
// context is derived from the caller's context (not the QUIC stream's send-side
// context, which would be canceled by CloseSend while we are still receiving the
// response). watchCaller bridges caller-context cancellation to a QUIC reset,
// since QUIC has no awareness of Go contexts.
func newClientStream(callerCtx context.Context, stream *quic.Stream, maxMsg int) *QuicStream {
	ctx, cancel := context.WithCancel(callerCtx)
	return &QuicStream{
		stream: stream,
		reader: newReader(stream, maxMsg),
		kind:   drpc.StreamKindClient,
		ctx:    ctx,
		cancel: cancel,
	}
}

// Context returns the context associated with the stream.
func (s *QuicStream) Context() context.Context { return s.ctx }

// Kind returns whether this is a client or server stream.
func (s *QuicStream) Kind() drpc.StreamKind { return s.kind }

// watchCaller arranges for the QUIC stream to be reset when callerCtx is
// canceled, so the remote (server) observes the client's cancellation and a
// blocked MsgRecv returns. Used on the client side only.
func (s *QuicStream) watchCaller(callerCtx context.Context) {
	s.stop = context.AfterFunc(callerCtx, func() { _ = s.Close() })
}

// writeFrame writes a single packet to the QUIC stream as one drpcwire frame.
// Writes are serialized and each packet gets a monotonically increasing message
// id starting at 1, as required by drpcwire.Reader.ReadPacket. The QUIC stream
// is the identity, so the stream id is a constant 1.
func (s *QuicStream) writeFrame(kind drpcwire.Kind, data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.sendDone {
		return errs.New("send closed")
	}
	s.sendID++
	buf := drpcwire.AppendFrame(nil, drpcwire.Frame{
		Data: data,
		ID:   drpcwire.ID{Stream: 1, Message: s.sendID},
		Kind: kind,
		Done: true,
	})
	_, err := s.stream.Write(buf)
	return err
}

// MsgSend marshals msg with enc and writes it as a KindMessage packet.
func (s *QuicStream) MsgSend(msg drpc.Message, enc drpc.Encoding) error {
	data, err := drpcenc.MarshalAppend(msg, enc, nil)
	if err != nil {
		return err
	}
	return s.writeFrame(drpcwire.KindMessage, data)
}

// MsgRecv reads the next packet and decodes it into msg. A KindError packet is
// returned as an error (preserving its code), and an end-of-stream (peer FIN)
// is returned as io.EOF. Once a terminal condition is hit it is returned on
// every subsequent call.
func (s *QuicStream) MsgRecv(msg drpc.Message, enc drpc.Encoding) error {
	if s.recvErr != nil {
		return s.recvErr
	}
	for {
		pkt, err := s.reader.ReadPacket()
		if err != nil {
			if errs.Is(err, io.EOF) || errs.Is(err, io.ErrUnexpectedEOF) {
				err = io.EOF
			}
			s.recvErr = err
			return err
		}
		switch pkt.Kind {
		case drpcwire.KindMessage:
			return enc.Unmarshal(pkt.Data, msg)
		case drpcwire.KindError:
			s.recvErr = drpcwire.UnmarshalError(pkt.Data)
			return s.recvErr
		default:
			// Ignore unknown control packets; otherwise it's a protocol error.
			if pkt.Control {
				continue
			}
			s.recvErr = drpc.ProtocolError.New("unexpected packet kind %s", pkt.Kind)
			return s.recvErr
		}
	}
}

// CloseSend signals to the remote that we will no longer send any messages by
// closing the QUIC stream's send side (FIN). The receive side stays open.
func (s *QuicStream) CloseSend() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.sendDone {
		return nil
	}
	s.sendDone = true
	return s.stream.Close()
}

// Close tears down both directions of the stream and cancels its context.
func (s *QuicStream) Close() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.stop != nil {
		s.stop()
	}
	s.stream.CancelRead(quicCancelCode)
	if !s.sendDone {
		s.sendDone = true
		s.stream.CancelWrite(quicCancelCode)
	}
	// Server streams use the QUIC stream's own context (canceled natively by the
	// CancelRead/CancelWrite above); only client streams have a cancel func.
	if s.cancel != nil {
		s.cancel()
	}
	return nil
}

// writeInvoke writes the optional metadata packet followed by the invoke packet
// naming the rpc. Used by the client when starting a stream.
func (s *QuicStream) writeInvoke(rpc string, metadata []byte) error {
	if len(metadata) > 0 {
		if err := s.writeFrame(drpcwire.KindInvokeMetadata, metadata); err != nil {
			return err
		}
	}
	return s.writeFrame(drpcwire.KindInvoke, []byte(rpc))
}

// readInvoke reads leading metadata packets and the invoke packet, returning
// the rpc name and enriching the stream's context with any incoming metadata.
// Used by the server on a freshly accepted stream.
func (s *QuicStream) readInvoke() (rpc string, err error) {
	for {
		pkt, err := s.reader.ReadPacket()
		if err != nil {
			return "", err
		}
		switch pkt.Kind {
		case drpcwire.KindInvokeMetadata:
			md, err := drpcmetadata.Decode(pkt.Data)
			if err != nil {
				return "", err
			}
			if s.grpcMetadataCompat {
				// Populate incoming metadata as gRPC metadata, mirroring the TCP
				// path's GRPCMetadataCompatMode so callers need no code changes.
				grpcMeta := make(map[string][]string, len(md))
				for k, v := range md {
					grpcMeta[k] = []string{v}
				}
				s.ctx = grpcmetadata.NewIncomingContext(s.ctx, grpcMeta)
			} else {
				s.ctx = drpcmetadata.NewIncomingContext(s.ctx, md)
			}
		case drpcwire.KindInvoke:
			return string(pkt.Data), nil
		default:
			if pkt.Control {
				continue
			}
			return "", drpc.ProtocolError.New("expected invoke, got %s", pkt.Kind)
		}
	}
}

// sendError writes serr to the remote as a KindError packet, preserving codes.
func (s *QuicStream) sendError(serr error) error {
	return s.writeFrame(drpcwire.KindError, drpcwire.MarshalError(serr))
}

// ServeStream reads the invoke header from an accepted QUIC stream, dispatches
// the rpc to handler, and reports any handler error back over the stream before
// closing the send side. maxMsg bounds a single received message (0 = default);
// grpcCompat decodes incoming metadata as gRPC metadata. The handler's Context()
// is the QUIC stream's own context, canceled natively when the client resets it.
func ServeStream(
	stream *quic.Stream, handler drpc.Handler, peerInfo drpcctx.PeerConnectionInfo,
	maxMsg int, grpcCompat bool,
) (err error) {
	s := newServerStream(stream, peerInfo, maxMsg, grpcCompat)

	rpc, err := s.readInvoke()
	if err != nil {
		// Nothing was sent yet; abandon the stream.
		return errs.Wrap(errs.Combine(err, s.Close()))
	}

	// On both the success and handler-error paths we half-close (FIN) rather
	// than reset: a reset (RST_STREAM) can cause the peer to discard buffered
	// response messages or the error frame that we just sent. The client closes
	// its end when it is done reading.
	if herr := handler.HandleRPC(s, rpc); herr != nil {
		return errs.Wrap(errs.Combine(s.sendError(herr), s.CloseSend()))
	}
	return errs.Wrap(s.CloseSend())
}
