// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcmanager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	grpcmetadata "google.golang.org/grpc/metadata"

	"storj.io/drpc"
	"storj.io/drpc/drpcdebug"
	"storj.io/drpc/drpcmetadata"
	"storj.io/drpc/drpcsignal"
	"storj.io/drpc/drpcstream"
	"storj.io/drpc/drpcwire"
	"storj.io/drpc/internal/drpcopts"
)

// MuxManager handles the logic of managing a transport for a drpc client or
// server with stream multiplexing enabled. Multiple streams can be active
// concurrently on a single transport.
type MuxManager struct {
	tr   drpc.Transport
	rd   *drpcwire.Reader
	opts Options

	sw       *sharedWriteBuf
	reg      *streamRegistry
	streamID atomic.Uint64
	wg       sync.WaitGroup
	pkts     chan drpcwire.Packet
	pdone    drpcsignal.Chan
	metaMu   sync.Mutex
	meta     map[uint64]map[string]string

	sigs struct {
		term  drpcsignal.Signal
		write drpcsignal.Signal
		read  drpcsignal.Signal
		tport drpcsignal.Signal
	}
}

// NewMuxWithOptions returns a new mux manager for the transport. It uses the
// provided options to manage details of how it uses it.
func NewMuxWithOptions(tr drpc.Transport, opts Options) *MuxManager {
	m := &MuxManager{
		tr:   tr,
		rd:   drpcwire.NewReaderWithOptions(tr, opts.Reader),
		opts: opts,

		pkts: make(chan drpcwire.Packet),
		meta: make(map[uint64]map[string]string),
	}

	// a buffer of size 1 allows the consumer of the packet to signal it is done
	// without having to coordinate with the sender of the packet.
	m.pdone.Make(1)

	// set the internal stream options
	drpcopts.SetStreamTransport(&m.opts.Stream.Internal, m.tr)
	drpcopts.SetStreamMux(&m.opts.Stream.Internal, true)

	m.sw = newSharedWriteBuf()
	m.reg = newStreamRegistry()

	go m.manageReader()
	go m.manageWriter()

	return m
}

// String returns a string representation of the manager.
func (m *MuxManager) String() string { return fmt.Sprintf("<mux %p>", m) }

func (m *MuxManager) log(what string, cb func() string) {
	if drpcdebug.Enabled {
		drpcdebug.Log(func() (_, _, _ string) { return m.String(), what, cb() })
	}
}

//
// helpers
//

// terminate puts the MuxManager into a terminal state and closes any resources
// that need to be closed to signal the state change.
func (m *MuxManager) terminate(err error) {
	if m.sigs.term.Set(err) {
		m.log("TERM", func() string { return fmt.Sprint(err) })
		m.sigs.tport.Set(m.tr.Close())
		m.sw.Close()
		m.metaMu.Lock()
		for id := range m.meta {
			delete(m.meta, id)
		}
		m.metaMu.Unlock()
		// Cancel all active streams so they get a clear error.
		m.reg.ForEach(func(_ uint64, s *drpcstream.Stream) {
			cancelErr := err
			if errors.Is(cancelErr, io.EOF) {
				cancelErr = context.Canceled
				if s.Kind() == drpc.StreamKindClient {
					cancelErr = drpc.ClosedError.New("connection closed")
				}
			}
			s.Cancel(cancelErr)
		})
		m.reg.Close()
	}
}

func (m *MuxManager) putMetadata(streamID uint64, metadata map[string]string) {
	m.metaMu.Lock()
	defer m.metaMu.Unlock()
	m.meta[streamID] = metadata
}

func (m *MuxManager) popMetadata(streamID uint64) map[string]string {
	m.metaMu.Lock()
	defer m.metaMu.Unlock()
	metadata := m.meta[streamID]
	delete(m.meta, streamID)
	return metadata
}

//
// manage reader
//

// manageReader is always reading a packet and dispatching it to the appropriate
// stream or queue. It sets the read signal when it exits so that one can wait
// to ensure that no one is reading on the reader. It sets the term signal if
// there is any error reading packets.
func (m *MuxManager) manageReader() {
	defer m.sigs.read.Set(nil)

	var pkt drpcwire.Packet
	var err error
	var run int

	for !m.sigs.term.IsSet() {
		// if we have a run of "small" packets, drop the buffer to release
		// memory so that a burst of large packets does not cause eternally
		// large heap usage.
		if run > 10 {
			pkt.Data = nil
			run = 0
		}

		pkt, err = m.rd.ReadPacketUsing(pkt.Data[:0])
		if err != nil {
			if isConnectionReset(err) {
				err = drpc.ClosedError.Wrap(err)
			}
			m.terminate(managerClosed.Wrap(err))
			return
		}

		if len(pkt.Data) < cap(pkt.Data)/4 {
			run++
		} else {
			run = 0
		}

		m.log("READ", pkt.String)

		stream, ok := m.reg.Get(pkt.ID.Stream)

		switch {
		// if the packet is for a registered stream, deliver it.
		case ok && stream != nil:
			if err := stream.HandlePacket(pkt); err != nil {
				m.terminate(managerClosed.Wrap(err))
				return
			}
			// For message packets, HandlePacket transferred ownership of
			// pkt.Data to the stream's packetBuffer. Acquire a fresh buffer
			// from the pool so the next ReadPacketUsing doesn't allocate.
			if pkt.Kind == drpcwire.KindMessage {
				pkt.Data = drpcstream.AcquirePacketBuf()
			}

		// if any invoke sequence is being sent, forward it to be handled.
		case pkt.Kind == drpcwire.KindInvoke || pkt.Kind == drpcwire.KindInvokeMetadata:
			select {
			case m.pkts <- pkt:
				m.pdone.Recv()
			case <-m.sigs.term.Signal():
				return
			}

		// silently drop packet for an unregistered stream
		default:
			m.log("DROP", pkt.String)
		}
	}
}

// manageWriter drains the shared write buffer and writes pre-serialized
// bytes directly to the transport. It blocks on the sharedWriteBuf's
// condition variable until data is available, and naturally batches
// frames that accumulate while the previous write is in flight.
func (m *MuxManager) manageWriter() {
	defer m.sigs.write.Set(nil)

	var spare []byte
	for {
		data, ok := m.sw.WaitAndDrain(spare[:0:cap(spare)])
		if !ok {
			return
		}
		if _, err := m.tr.Write(data); err != nil {
			m.terminate(managerClosed.Wrap(err))
			return
		}
		spare = data
	}
}

//
// manage streams
//

// newStream creates a stream value with the appropriate configuration for this manager.
func (m *MuxManager) newStream(ctx context.Context, sid uint64, kind drpc.StreamKind, rpc string) (*drpcstream.Stream, error) {
	opts := m.opts.Stream
	drpcopts.SetStreamKind(&opts.Internal, kind)
	drpcopts.SetStreamRPC(&opts.Internal, rpc)
	if cb := drpcopts.GetManagerStatsCB(&m.opts.Internal); cb != nil {
		drpcopts.SetStreamStats(&opts.Internal, cb(rpc))
	}

	stream := drpcstream.NewWithOptions(ctx, sid, &muxWriter{sw: m.sw}, opts)

	if err := m.reg.Register(sid, stream); err != nil {
		return nil, err
	}

	m.wg.Add(1)
	go m.manageStream(ctx, stream)

	m.log("STREAM", stream.String)
	return stream, nil
}

// manageStream watches the context and the stream and returns when the stream
// is finished, canceling the stream if the context is canceled.
func (m *MuxManager) manageStream(ctx context.Context, stream *drpcstream.Stream) {
	defer m.wg.Done()
	defer m.reg.Unregister(stream.ID())

	select {
	case <-m.sigs.term.Signal():
		err := m.sigs.term.Err()
		if errors.Is(err, io.EOF) {
			err = context.Canceled
			if stream.Kind() == drpc.StreamKindClient {
				err = drpc.ClosedError.New("connection closed")
			}
		}
		stream.Cancel(err)
		<-stream.Finished()

	case <-stream.Finished():
		// stream finished naturally

	case <-ctx.Done():
		m.log("CANCEL", stream.String)

		if m.opts.SoftCancel {
			// Best-effort send KindCancel, never terminate connection.
			if busy, err := stream.SendCancel(ctx.Err()); err != nil {
				m.log("CANCEL_ERR", func() string {
					return fmt.Sprintf("%s: %v", stream.String(), err)
				})
			} else if busy {
				m.log("CANCEL_BUSY", stream.String)
			}
			stream.Cancel(ctx.Err())
			<-stream.Finished()
		} else {
			// Hard cancel: terminate connection if stream not finished.
			if !stream.Cancel(ctx.Err()) {
				m.log("UNFIN", stream.String)
				m.terminate(ctx.Err())
			} else {
				m.log("CLEAN", stream.String)
			}
			<-stream.Finished()
		}
	}
}

//
// exported interface
//

// Closed returns a channel that is closed once the manager is closed.
func (m *MuxManager) Closed() <-chan struct{} {
	return m.sigs.term.Signal()
}

// Unblocked returns a channel that is closed when the manager is available for
// new streams. With multiplexing enabled, the connection is never blocked, so
// this always returns an already-closed channel.
func (m *MuxManager) Unblocked() <-chan struct{} {
	return closedCh
}

// Close closes the transport the manager is using.
func (m *MuxManager) Close() error {
	m.terminate(managerClosed.New("Close called"))

	m.wg.Wait() // wait for all stream goroutines
	m.sigs.write.Wait()
	m.sigs.read.Wait()
	m.sigs.tport.Wait()

	return m.sigs.tport.Err()
}

// NewClientStream starts a stream on the managed transport for use by a client.
func (m *MuxManager) NewClientStream(ctx context.Context, rpc string) (stream *drpcstream.Stream, err error) {
	if err, ok := m.sigs.term.Get(); ok {
		return nil, err
	}
	sid := m.streamID.Add(1)
	return m.newStream(ctx, sid, drpc.StreamKindClient, rpc)
}

// NewServerStream starts a stream on the managed transport for use by a server.
// It does this by waiting for the client to issue an invoke message and
// returning the details.
func (m *MuxManager) NewServerStream(ctx context.Context) (stream *drpcstream.Stream, rpc string, err error) {
	if err, ok := m.sigs.term.Get(); ok {
		return nil, "", err
	}

	var timeoutCh <-chan time.Time

	// set up the timeout on the context if necessary.
	if timeout := m.opts.InactivityTimeout; timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		timeoutCh = timer.C
	}

	for {
		select {
		case <-timeoutCh:
			return nil, "", context.DeadlineExceeded

		case <-ctx.Done():
			return nil, "", ctx.Err()

		case <-m.sigs.term.Signal():
			return nil, "", m.sigs.term.Err()

		case pkt := <-m.pkts:
			switch pkt.Kind {
			case drpcwire.KindInvokeMetadata:
				metadata, err := drpcmetadata.Decode(pkt.Data)
				if err != nil {
					m.pdone.Send()
					return nil, "", err
				}
				m.putMetadata(pkt.ID.Stream, metadata)
				m.pdone.Send()

			case drpcwire.KindInvoke:
				rpc = string(pkt.Data)
				streamCtx := ctx

				if metadata := m.popMetadata(pkt.ID.Stream); metadata != nil {
					if m.opts.GRPCMetadataCompatMode {
						grpcMeta := make(map[string][]string, len(metadata))
						for k, v := range metadata {
							grpcMeta[k] = []string{v}
						}
						streamCtx = grpcmetadata.NewIncomingContext(streamCtx, grpcMeta)
					} else {
						streamCtx = drpcmetadata.NewIncomingContext(streamCtx, metadata)
					}
				}
				stream, err := m.newStream(streamCtx, pkt.ID.Stream, drpc.StreamKindServer, rpc)
				// Ack the invoke only after stream registration so subsequent
				// message packets cannot be dropped for an unknown stream ID.
				m.pdone.Send()
				return stream, rpc, err

			default:
				// this should never happen, but defensive.
				m.pdone.Send()
			}
		}
	}
}
