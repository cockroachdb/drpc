// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcmanager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/zeebo/errs"
	grpcmetadata "google.golang.org/grpc/metadata"

	"storj.io/drpc"
	"storj.io/drpc/drpcdebug"
	"storj.io/drpc/drpcmetadata"
	"storj.io/drpc/drpcsignal"
	"storj.io/drpc/drpcstream"
	"storj.io/drpc/drpcwire"
	"storj.io/drpc/internal/drpcopts"
)

var managerClosed = errs.Class("manager closed")

// Options controls configuration settings for a manager.
type Options struct {
	// WriterBufferSize controls the size of the buffer that we will fill before
	// flushing. Normal writes to streams typically issue a flush explicitly.
	WriterBufferSize int

	// Reader are passed to any readers the manager creates.
	Reader drpcwire.ReaderOptions

	// Stream are passed to any streams the manager creates.
	Stream drpcstream.Options

	// SoftCancel controls if a context cancel will cause the transport to be
	// closed or, if true, a soft cancel message will be attempted if possible.
	// A soft cancel can reduce the amount of closed and dialed connections at
	// the potential cost of higher latencies if there is latent data still
	// being flushed when the cancel happens.
	SoftCancel bool

	// InactivityTimeout is the amount of time the manager will wait when
	// creating a NewServerStream. It only includes the time it is reading
	// packets from the remote client. In other words, it only includes the time
	// that the client could delay before invoking an RPC. If zero or negative,
	// no timeout is used.
	InactivityTimeout time.Duration

	// Internal contains options that are for internal use only.
	Internal drpcopts.Manager

	// GRPCMetadataCompatMode enables/disable gRPC compatibility for metadata
	// handling. When enabled, the server stream will decode incoming metadata
	// into grpc metadata in the context.
	GRPCMetadataCompatMode bool
}

// Manager handles the logic of managing a transport for a drpc client or
// server. It ensures that the connection is always being read from, that it is
// closed in the case that the manager is and forwarding drpc protocol messages
// to the appropriate stream.
type Manager struct {
	tr   drpc.Transport
	rd   *drpcwire.Reader
	opts Options

	sw       *sharedWriteBuf      // shared write buffer for the writer goroutine
	reg      *streamRegistry      // tracks all active streams by ID
	streamID atomic.Uint64        // next stream ID for client streams
	wg       sync.WaitGroup       // tracks active manageStream goroutines
	pkts     chan drpcwire.Packet // channel for invoke packets
	pdone    drpcsignal.Chan      // signals when packet buffers can be reused
	metaMu   sync.Mutex
	meta     map[uint64]map[string]string // invoke metadata buffered by stream ID

	sigs struct {
		term  drpcsignal.Signal // set when the manager should start terminating
		write drpcsignal.Signal // set when the writer goroutine is done
		read  drpcsignal.Signal // set after the goroutine reading from the transport is done
		tport drpcsignal.Signal // set after the transport has been closed
	}
}

// New returns a new Manager for the transport.
func New(tr drpc.Transport) *Manager {
	return NewWithOptions(tr, Options{})
}

// NewWithOptions returns a new manager for the transport. It uses the provided
// options to manage details of how it uses it.
func NewWithOptions(tr drpc.Transport, opts Options) *Manager {
	m := &Manager{
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

	m.sw = newSharedWriteBuf()
	m.reg = newStreamRegistry()

	go m.manageReader()
	go m.manageWriter()

	return m
}

// String returns a string representation of the manager.
func (m *Manager) String() string { return fmt.Sprintf("<man %p>", m) }

func (m *Manager) log(what string, cb func() string) {
	if drpcdebug.Enabled {
		drpcdebug.Log(func() (_, _, _ string) { return m.String(), what, cb() })
	}
}

//
// helpers
//

// terminate puts the Manager into a terminal state and closes any resources
// that need to be closed to signal the state change.
func (m *Manager) terminate(err error) {
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
		cancelErr := err
		if errors.Is(cancelErr, io.EOF) {
			cancelErr = context.Canceled
		}
		m.reg.ForEach(func(_ uint64, s *drpcstream.Stream) {
			s.Cancel(cancelErr)
		})
		m.reg.Close()
	}
}

func (m *Manager) putMetadata(streamID uint64, metadata map[string]string) {
	m.metaMu.Lock()
	defer m.metaMu.Unlock()
	m.meta[streamID] = metadata
}

func (m *Manager) popMetadata(streamID uint64) map[string]string {
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
func (m *Manager) manageReader() {
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
func (m *Manager) manageWriter() {
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
func (m *Manager) newStream(ctx context.Context, sid uint64, kind, rpc string) (*drpcstream.Stream, error) {
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
func (m *Manager) manageStream(ctx context.Context, stream *drpcstream.Stream) {
	defer m.wg.Done()
	defer m.reg.Unregister(stream.ID())

	select {
	case <-m.sigs.term.Signal():
		err := m.sigs.term.Err()
		if errors.Is(err, io.EOF) {
			err = context.Canceled
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
func (m *Manager) Closed() <-chan struct{} {
	return m.sigs.term.Signal()
}

// Unblocked returns a channel that is closed when the manager is available for
// new streams. With multiplexing enabled, the connection is never blocked, so
// this always returns an already-closed channel.
func (m *Manager) Unblocked() <-chan struct{} {
	return closedCh
}

// Close closes the transport the manager is using.
func (m *Manager) Close() error {
	m.terminate(managerClosed.New("Close called"))

	m.wg.Wait() // wait for all stream goroutines
	m.sigs.write.Wait()
	m.sigs.read.Wait()
	m.sigs.tport.Wait()

	return m.sigs.tport.Err()
}

// NewClientStream starts a stream on the managed transport for use by a client.
func (m *Manager) NewClientStream(ctx context.Context, rpc string) (stream *drpcstream.Stream, err error) {
	if err, ok := m.sigs.term.Get(); ok {
		return nil, err
	}
	sid := m.streamID.Add(1)
	return m.newStream(ctx, sid, "cli", rpc)
}

// NewServerStream starts a stream on the managed transport for use by a server.
// It does this by waiting for the client to issue an invoke message and
// returning the details.
func (m *Manager) NewServerStream(ctx context.Context) (stream *drpcstream.Stream, rpc string, err error) {
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
						// Populate incoming metadata as grpc metadata in the
						// context. This is a short-term fix that will enable us
						// to send and receive grpc metadata when DRPC is enabled,
						// without any changes in the calling code.
						grpcMeta := make(map[string][]string, len(metadata))
						for k, v := range metadata {
							grpcMeta[k] = []string{v}
						}
						streamCtx = grpcmetadata.NewIncomingContext(streamCtx, grpcMeta)
					} else {
						// Add metadata to the incoming context.
						streamCtx = drpcmetadata.NewIncomingContext(streamCtx, metadata)
					}
				}
				stream, err := m.newStream(streamCtx, pkt.ID.Stream, "srv", rpc)
				// Ack the invoke only after stream registration so subsequent
				// message packets cannot be dropped for an unknown stream ID.
				// Always ack, even on error, so the reader goroutine does not block.
				m.pdone.Send()
				return stream, rpc, err

			default:
				// this should never happen, but defensive.
				m.pdone.Send()
			}
		}
	}
}

func isConnectionReset(err error) bool {
	var operr *net.OpError
	if !errors.As(err, &operr) {
		return false
	}
	if errors.Is(operr.Err, syscall.ECONNRESET) {
		return true
	}
	msg := strings.ToLower(operr.Err.Error())
	if strings.Contains(msg, "connection reset by peer") {
		return true
	}
	if strings.Contains(msg, "connection was forcibly closed by the remote host") {
		return true
	}
	if strings.Contains(msg, strings.ToLower(syscall.ECONNRESET.Error())) {
		return true
	}
	return false
}
