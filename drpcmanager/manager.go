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
	wr   *drpcwire.Writer
	rd   *drpcwire.Reader
	opts Options

	// next client stream ID, incremented atomically
	lastStreamID atomic.Uint64

	wg sync.WaitGroup // tracks active manageStream goroutines

	// reg tracks active streams. Currently holds at most one active stream;
	// a second may briefly coexist during stream handoff (old stream's
	// Unregister races with new stream's Register).
	reg *streamRegistry

	pdone   drpcsignal.Chan // signals when NewServerStream has registered the new stream
	invokes chan invokeInfo // completed invoke info from manageReader to NewServerStream

	// invokesAssembler is owned by the manageReader goroutine, used in
	// handleInvokeFrame.
	invokesAssembler map[uint64]*invokeAssembler

	sigs struct {
		term  drpcsignal.Signal // set when the manager should start terminating
		read  drpcsignal.Signal // set after the goroutine reading from the transport is done
		tport drpcsignal.Signal // set after the transport has been closed
	}
}

type invokeAssembler struct {
	metadata map[string]string        // accumulated invoke metadata
	pa       drpcwire.PacketAssembler // assembles invoke/metadata frames into packets
}

// invokeInfo carries the assembled invoke data from manageReader to
// NewServerStream. It is reused across invocations; call Reset between uses.
type invokeInfo struct {
	sid      uint64
	metadata map[string]string
	data     []byte // RPC name bytes from the KindInvoke packet
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
		wr:   drpcwire.NewWriter(tr, opts.WriterBufferSize),
		rd:   drpcwire.NewReaderWithOptions(tr, opts.Reader),
		opts: opts,

		invokes: make(chan invokeInfo),
	}

	// a buffer of size 1 allows NewServerStream to signal it is done creating a
	// new server stream without having to coordinate with manageReader.
	m.pdone.Make(1)

	m.invokesAssembler = make(map[uint64]*invokeAssembler)

	m.reg = newStreamRegistry()

	// set the internal stream options
	drpcopts.SetStreamTransport(&m.opts.Stream.Internal, m.tr)

	go m.manageReader()

	return m
}

// String returns a string representation of the manager.
func (m *Manager) String() string { return fmt.Sprintf("<man %p>", m) }

func (m *Manager) log(what string, cb func() string) {
	if drpcdebug.Enabled {
		drpcdebug.Log(func() (_, _, _ string) { return m.String(), what, cb() })
	}
}

// terminate puts the Manager into a terminal state and closes any resources
// that need to be closed to signal the state change.
func (m *Manager) terminate(err error) {
	if m.sigs.term.Set(err) {
		m.log("TERM", func() string { return fmt.Sprint(err) })
		m.sigs.tport.Set(m.tr.Close())
		m.reg.Close()
	}
}

//
// manage reader
//

// manageReader reads the frame and dispatches them to the appropriate stream or
// queue. It sets the read signal when it exits so that one can wait to ensure
// that no one is reading on the reader. It sets the term signal if there is any
// error reading frames.
func (m *Manager) manageReader() {
	defer m.sigs.read.Set(nil)

	for !m.sigs.term.IsSet() {
		incomingFrame, err := m.rd.ReadFrame()
		if err != nil {
			if isConnectionReset(err) {
				err = drpc.ClosedError.Wrap(err)
			}
			m.terminate(managerClosed.Wrap(err))
			return
		}

		m.log("READ", incomingFrame.String)

		stream, ok := m.reg.Get(incomingFrame.ID.Stream)

		switch {
		// if the packet is for a registered stream, deliver it.
		case ok && stream != nil:
			if err := stream.HandleFrame(incomingFrame); err != nil {
				m.terminate(managerClosed.Wrap(err))
				return
			}

		case incomingFrame.Kind == drpcwire.KindInvoke || incomingFrame.Kind == drpcwire.KindInvokeMetadata:
			if err := m.handleInvokeFrame(incomingFrame); err != nil {
				m.terminate(managerClosed.Wrap(err))
				return
			}

		// silently drop packet for an unregistered stream
		default:
			m.log("DROP", incomingFrame.String)
		}
	}
}

// handleInvokeFrame assembles invoke/metadata frames into complete packets and
// forwards the finished invoke info to NewServerStream via m.newServerStreamInfo.
// Metadata packets are accumulated; the invoke packet triggers the send.
func (m *Manager) handleInvokeFrame(fr drpcwire.Frame) error {
	ia, ok := m.invokesAssembler[fr.ID.Stream]
	if !ok {
		ia = &invokeAssembler{pa: drpcwire.NewPacketAssembler()}
		m.invokesAssembler[fr.ID.Stream] = ia
	}
	pkt, packetReady, err := ia.pa.AppendFrame(fr)
	if err != nil {
		return err
	}
	if !packetReady {
		return nil
	}

	// Metadata arrives before invoke; accumulate it and wait for the invoke.
	if pkt.Kind == drpcwire.KindInvokeMetadata {
		meta, err := drpcmetadata.Decode(pkt.Data)
		if err != nil {
			return err
		}
		ia.metadata = meta
		return nil
	}

	// Invoke packet completes the sequence. Send to NewServerStream.
	select {
	case m.invokes <- invokeInfo{sid: pkt.ID.Stream, data: pkt.Data, metadata: ia.metadata}:
		// Wait for NewServerStream to finish stream creation before reading the
		// next frame. This guarantees curr is set for subsequent non-invoke
		// packets.
		m.pdone.Recv()
		// TODO: reuse invoke assembler
		delete(m.invokesAssembler, fr.ID.Stream)
	case <-m.sigs.term.Signal():
	}
	return nil
}

//
// manage streams
//

// newStream creates a stream value with the appropriate configuration for this manager.
func (m *Manager) newStream(ctx context.Context, sid uint64, kind drpc.StreamKind, rpc string) (*drpcstream.Stream, error) {
	opts := m.opts.Stream
	drpcopts.SetStreamKind(&opts.Internal, kind)
	drpcopts.SetStreamRPC(&opts.Internal, rpc)
	if cb := drpcopts.GetManagerStatsCB(&m.opts.Internal); cb != nil {
		drpcopts.SetStreamStats(&opts.Internal, cb(rpc))
	}

	stream := drpcstream.NewWithOptions(ctx, sid, m.wr, opts)

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
			if stream.Kind() == drpc.StreamKindClient {
				err = drpc.ClosedError.New("connection closed")
			}
		}
		stream.Cancel(err)
		<-stream.Finished()

	case <-stream.Finished():

	case <-ctx.Done():
		m.log("CANCEL", stream.String)

		if err := stream.SendCancel(ctx.Err()); err != nil {
			// SendCancel can fail if it's an IO error which reader will catch.
			m.log("SendCancel", func() string { return fmt.Sprintf("%s: %s", stream.String(), err) })
		}
		stream.Cancel(ctx.Err())
		<-stream.Finished()
	}
}

//
// exported interface
//

// Closed returns a channel that is closed once the manager is closed.
func (m *Manager) Closed() <-chan struct{} {
	return m.sigs.term.Signal()
}

// Unblocked returns a channel that is closed when the manager is no longer
// blocked from creating a new stream due to a previous stream's soft cancel. It
// should not be called concurrently with NewClientStream or NewServerStream and
// the return result is only valid until the next call to NewClientStream or
// NewServerStream.
func (m *Manager) Unblocked() <-chan struct{} {
	return closedCh
}

// Close closes the transport the manager is using.
func (m *Manager) Close() error {
	m.terminate(managerClosed.New("Close called"))

	m.wg.Wait() // wait for all stream goroutines
	m.sigs.read.Wait()
	m.sigs.tport.Wait()

	return m.sigs.tport.Err()
}

// NewClientStream starts a stream on the managed transport for use by a client.
func (m *Manager) NewClientStream(ctx context.Context, rpc string) (stream *drpcstream.Stream, err error) {
	return m.newStream(ctx, m.lastStreamID.Add(1), drpc.StreamKindClient, rpc)
}

// NewServerStream starts a stream on the managed transport for use by a server.
// It does this by waiting for the client to issue an invoke message and
// returning the details.
func (m *Manager) NewServerStream(ctx context.Context) (stream *drpcstream.Stream, rpc string, err error) {
	select {
	case <-ctx.Done():
		return nil, "", ctx.Err()

	case <-m.sigs.term.Signal():
		return nil, "", m.sigs.term.Err()

	case pkt := <-m.invokes:
		rpc = string(pkt.data)
		if pkt.metadata != nil {
			if m.opts.GRPCMetadataCompatMode {
				// Populate incoming metadata as grpc metadata in the
				// context. This is a short-term fix that will enable us
				// to send and receive grpc metadata when DRPC is enabled,
				// without any changes in the calling code.
				grpcMeta := make(map[string][]string, len(pkt.metadata))
				for k, v := range pkt.metadata {
					grpcMeta[k] = []string{v}
				}
				ctx = grpcmetadata.NewIncomingContext(ctx, grpcMeta)
			} else {
				// Add metadata to the incoming context.
				ctx = drpcmetadata.NewIncomingContext(ctx, pkt.metadata)
			}
		}
		stream, err := m.newStream(ctx, pkt.sid, drpc.StreamKindServer, rpc)
		// Signal pdone only after stream registration so that manageReader sees
		// the new stream in the registry when it reads the next frame.
		m.pdone.Send()
		return stream, rpc, err
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
