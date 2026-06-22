// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcstream

import (
	"context"
	"fmt"
	"io"
	"runtime/trace"
	"sync"

	"github.com/zeebo/errs"
	"storj.io/drpc"
	"storj.io/drpc/drpcctx"
	"storj.io/drpc/drpcdebug"
	"storj.io/drpc/drpcenc"
	"storj.io/drpc/drpcsignal"
	"storj.io/drpc/drpcwire"
	"storj.io/drpc/internal/drpcopts"
)

// Options controls configuration settings for a stream.
type Options struct {
	// SplitSize controls the default size we split data packets into frames.
	SplitSize int

	// MaximumBufferSize causes the Stream to drop any internal buffers that are
	// larger than this amount to control maximum memory usage at the expense of
	// more allocations. 0 is unlimited.
	MaximumBufferSize int

	// Internal contains options that are for internal use only.
	Internal drpcopts.Stream
}

// Stream represents an rpc actively happening on a transport.
type Stream struct {
	ctx  streamCtx
	opts Options
	task *trace.Task

	// write and read serialize operations within a stream. The data path
	// (MsgSend/MsgRecv) and the control path (SendCancel/Close/SendError)
	// genuinely race because cancellation arrives from manageStream while the
	// application may be mid-send. These are inspectMutex (not sync.Mutex) so
	// that checkFinished can test whether ops are in flight without blocking.
	write inspectMutex
	read  inspectMutex

	pa drpcwire.PacketAssembler

	id        drpcwire.ID
	wr        *drpcwire.MuxWriter
	recvQueue ringBuffer
	wbuf      []byte

	mu   sync.Mutex // protects state transitions
	sigs struct {
		send drpcsignal.Signal // set when done sending messages
		recv drpcsignal.Signal // set when done receiving messages
		// Stream shutdown is two-phase: term then fin. When termination arrives
		// (remote error, local cancel, close), there may be an in-flight write
		// on the transport that is past the term check and inside WriteFrame.
		// term tells new operations to bail out; fin signals that all in-flight
		// operations have actually completed. Consumers (manageStream) wait on
		// fin before cleaning up, guaranteeing no goroutine is touching the
		// stream afterward.
		term   drpcsignal.Signal // set when the stream is terminating and no new ops should begin
		fin    drpcsignal.Signal // set when the stream is finished and all ops are complete
		cancel drpcsignal.Signal // set when externally canceled
	}
}

var _ drpc.Stream = (*Stream)(nil)

// New returns a new stream bound to the context with the given stream id and
// will use the writer to write messages on. It is important use monotonically
// increasing stream ids within a single transport.
func New(ctx context.Context, sid uint64, wr *drpcwire.MuxWriter, pool *BufferPool) *Stream {
	return NewWithOptions(ctx, sid, wr, pool, Options{})
}

// NewWithOptions returns a new stream bound to the context with the given
// stream id and will use the writer to write messages on. It is important use
// monotonically increasing stream ids within a single transport. The options
// are used to control details of how the Stream operates.
func NewWithOptions(
	ctx context.Context, sid uint64, wr *drpcwire.MuxWriter, pool *BufferPool, opts Options,
) *Stream {
	var task *trace.Task
	if trace.IsEnabled() {
		kind, rpc := drpcopts.GetStreamKind(&opts.Internal), drpcopts.GetStreamRPC(&opts.Internal)
		if kind != drpc.StreamKindUnknown && rpc != "" {
			ctx, task = trace.NewTask(ctx, kind.String()+rpc)
		}
	}

	pa := drpcwire.NewPacketAssembler()
	pa.SetStreamID(sid)

	s := &Stream{
		ctx: streamCtx{
			Context: ctx,
			tr:      drpcopts.GetStreamTransport(&opts.Internal),
		},
		opts: opts,
		task: task,

		pa: pa,

		id: drpcwire.ID{Stream: sid},
		wr: wr,
	}

	s.recvQueue.init(pool)

	return s
}

// String returns a string representation of the stream.
func (s *Stream) String() string {
	return fmt.Sprintf("<str %p s:%d k:%s r:%s>",
		s, s.id.Stream, drpcopts.GetStreamKind(&s.opts.Internal), drpcopts.GetStreamRPC(&s.opts.Internal),
	)
}

func (s *Stream) log(what string, cb func() string) {
	if drpcdebug.Enabled {
		drpcdebug.Log(func() (_, _, _ string) { return s.String(), what, cb() })
	}
	if s.task != nil {
		trace.Log(&s.ctx, what, cb())
	}
}

func (s *Stream) Kind() drpc.StreamKind {
	return drpcopts.GetStreamKind(&s.opts.Internal)
}

//
// context
//

// streamCtx avoids having to allocate a Done channel until it is requested.
type streamCtx struct {
	context.Context
	tr  drpc.Transport
	sig drpcsignal.Signal
}

// Value checks for the drpc.Transport key and forwards if necessary.
// We do this because using drpcctx to make a new context would cause
// an extra allocation.
func (s *streamCtx) Value(key interface{}) interface{} {
	if s.tr != nil && key == (drpcctx.TransportKey{}) {
		return s.tr
	}
	return s.Context.Value(key)
}

// Done returns the stored channel instead of the parent Done channel.
func (s *streamCtx) Done() <-chan struct{} { return s.sig.Signal() }

// Err returns the error that has been set when the done channel is closed.
func (s *streamCtx) Err() error { return s.sig.Err() }

// Context returns the context associated with the stream. It is closed when
// the Stream will no longer issue any writes or reads.
func (s *Stream) Context() context.Context { return &s.ctx }

//
// accessors
//

// ID returns the stream id.
func (s *Stream) ID() uint64 {
	if s == nil {
		return 0
	}
	return s.id.Stream
}

// Terminated returns a channel that is closed when the stream has been
// terminated.
func (s *Stream) Terminated() <-chan struct{} { return s.sigs.term.Signal() }

// IsTerminated returns true if the stream has been terminated.
func (s *Stream) IsTerminated() bool { return s.sigs.term.IsSet() }

// Finished returns a channel that is closed when the stream is fully finished
// and will no longer issue any writes or reads.
func (s *Stream) Finished() <-chan struct{} { return s.sigs.fin.Signal() }

// IsFinished returns true if the stream is fully finished and will no longer
// issue any writes or reads.
func (s *Stream) IsFinished() bool { return s.sigs.fin.IsSet() }

//
// frame handler
//

// HandleFrame processes an incoming frame, assembling multi-frame packets
// and dispatching complete packets to the stream state machine.
func (s *Stream) HandleFrame(fr drpcwire.Frame) (err error) {
	if s.sigs.term.IsSet() {
		return nil
	}

	packet, packetReady, err := s.pa.AppendFrame(fr)
	if err != nil {
		return err
	}
	if !packetReady {
		return nil
	}
	return s.handlePacket(packet)
}

// handlePacket advances the stream state machine by inspecting the packet. It
// returns any major errors that should terminate the transport the stream is
// operating on.
func (s *Stream) handlePacket(pkt drpcwire.Packet) (err error) {
	drpcopts.GetStreamStats(&s.opts.Internal).AddRead(uint64(len(pkt.Data)))

	s.log("HANDLE", pkt.String)

	if pkt.Kind == drpcwire.KindMessage {
		s.recvQueue.Enqueue(pkt.Data)
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	switch pkt.Kind {
	case drpcwire.KindInvoke, drpcwire.KindInvokeMetadata:
		err := drpc.ProtocolError.New("invoke on existing stream")
		s.terminate(err)
		return err

	case drpcwire.KindError:
		err := drpcwire.UnmarshalError(pkt.Data)
		s.sigs.send.Set(io.EOF) // in this state, gRPC returns io.EOF on send.
		s.terminate(err)
		return nil

	case drpcwire.KindCancel:
		err := context.Canceled
		s.sigs.cancel.Set(err)
		s.sigs.send.Set(io.EOF) // in this state, gRPC returns io.EOF on send.
		s.terminate(err)
		return nil

	case drpcwire.KindClose:
		s.sigs.recv.Set(io.EOF)
		s.recvQueue.Close(io.EOF)
		s.terminate(drpc.ClosedError.New("remote closed the stream"))
		return nil

	case drpcwire.KindCloseSend:
		s.sigs.recv.Set(io.EOF)
		s.recvQueue.Close(io.EOF)
		s.terminateIfBothClosed()
		return nil

	default:
		// ignore any unknown control packets for forwards compatibility
		if pkt.Control {
			return nil
		}

		err := drpc.InternalError.New("unknown packet kind: %s", pkt.Kind)
		s.terminate(err)
		return err
	}
}

//
// helpers
//

// checkFinished bridges the two-phase shutdown. It is called in two places:
// inside terminate() for when no I/O is in flight (fin fires immediately),
// and deferred after every read/write unlock for when an operation was in
// flight at termination time (fin fires once the last operation completes).
// Whichever call site runs last sees term set and both locks free, and sets fin.
func (s *Stream) checkFinished() {
	if s.sigs.term.IsSet() && s.write.Unlocked() && s.read.Unlocked() {
		if s.sigs.fin.Set(nil) {
			s.log("FIN", func() string { return "" })
			s.ctx.sig.Set(context.Canceled)
			if s.task != nil {
				s.task.End()
			}
		}
	}
}

// CheckCancelError will replace the error with one from the cancel signal if it
// is set. This is to prevent errors from reads/writes to a transport after it
// has been asynchronously closed due to context cancelation.
func (s *Stream) CheckCancelError(err error) error {
	if s.sigs.cancel.IsSet() {
		return s.sigs.cancel.Err()
	}
	return err
}

// newFrameLocked bumps the internal message id and returns a frame. It must be
// called under a mutex.
func (s *Stream) newFrameLocked(kind drpcwire.Kind) drpcwire.Frame {
	s.id.Message++
	return drpcwire.Frame{ID: s.id, Kind: kind}
}

// sendPacketLocked sends the packet in a single write. It does not check for
// any conditions to stop it from writing and is meant for internal stream use
// to do things like signal errors or closes to the remote side.
func (s *Stream) sendPacketLocked(kind drpcwire.Kind, control bool, data []byte) (err error) {
	fr := s.newFrameLocked(kind)
	fr.Data = data
	fr.Control = control
	fr.Done = true

	drpcopts.GetStreamStats(&s.opts.Internal).AddWritten(uint64(len(data)))
	s.log("SEND", fr.String)

	// Terminal frames are sent after terminate() has set the send signal, so we
	// must not pass that signal as WriteFrame's cancel: it is already set, and a
	// full buffer would make WriteFrame drop the frame, leaving the remote
	// without a clean Close/Error/Cancel. Passing nil lets the frame wait for
	// space so it always reaches the wire.
	if err := s.wr.WriteFrame(fr, nil); err != nil {
		return errs.Wrap(err)
	}
	return nil
}

// terminateIfBothClosed is a helper to terminate the stream if both sides have
// issued a CloseSend.
func (s *Stream) terminateIfBothClosed() {
	if s.sigs.send.IsSet() && s.sigs.recv.IsSet() {
		s.terminate(termBothClosed)
	}
}

// terminate marks the stream as terminated with the given error. It also marks
// the stream as finished if no writes are happening at the time of the call.
func (s *Stream) terminate(err error) {
	s.sigs.send.Set(err)
	s.sigs.recv.Set(err)
	s.sigs.term.Set(err)
	s.recvQueue.Close(err)
	s.checkFinished()
}

// WriteInvoke writes the invoke metadata (if any) and invoke frame
// atomically under the write lock. This prevents SendCancel from
// interrupting the invoke sequence.
func (s *Stream) WriteInvoke(rpc string, metadata []byte) error {
	defer s.checkFinished()
	s.write.Lock()
	defer s.write.Unlock()

	if len(metadata) > 0 {
		if err := s.rawWriteLocked(drpcwire.KindInvokeMetadata, metadata); err != nil {
			return err
		}
	}
	return s.rawWriteLocked(drpcwire.KindInvoke, []byte(rpc))
}

//
// raw read/write
//

// RawWrite sends the data bytes with the given kind.
func (s *Stream) RawWrite(kind drpcwire.Kind, data []byte) (err error) {
	defer s.checkFinished()
	s.write.Lock()
	defer s.write.Unlock()

	return s.rawWriteLocked(kind, data)
}

// rawWriteLocked does the body of RawWrite assuming the caller is holding the
// appropriate locks.
// TODO(shubham): can we merge this with sendPacketLocked?
func (s *Stream) rawWriteLocked(kind drpcwire.Kind, data []byte) (err error) {
	fr := s.newFrameLocked(kind)
	n := s.opts.SplitSize

	for {
		switch {
		case s.sigs.send.IsSet():
			return s.sigs.send.Err()
		case s.sigs.term.IsSet():
			return s.sigs.term.Err()
		}

		fr.Data, data = drpcwire.SplitData(data, n)
		fr.Done = len(data) == 0

		drpcopts.GetStreamStats(&s.opts.Internal).AddWritten(uint64(len(fr.Data)))
		s.log("SEND", fr.String)

		// Pass the send signal, the write side's own stop signal, so a parked
		// WriteFrame is woken when sending ends (cancel, error, close) and
		// returns that signal's error directly. That is io.EOF in the cancel and
		// error cases, the same error the loop guard above returns, so the parked
		// and unparked paths agree and there is nothing to remap here.
		if err := s.wr.WriteFrame(fr, &s.sigs.send); err != nil {
			return err
		} else if fr.Done {
			return nil
		}
	}
}

// RawRecv returns the raw bytes received for a message.
func (s *Stream) RawRecv() (data []byte, err error) {
	defer s.checkFinished()
	s.read.Lock()
	defer s.read.Unlock()

	b, err := s.recvQueue.Dequeue()
	if err != nil {
		return nil, err
	}
	data = append([]byte(nil), b...)
	s.recvQueue.Done()

	return data, nil
}

//
// msg read/write
//

// MsgSend marshals the message with the encoding and writes it.
func (s *Stream) MsgSend(msg drpc.Message, enc drpc.Encoding) (err error) {
	defer func() { err = drpc.ToRPCErr(err) }()
	defer s.checkFinished()
	s.write.Lock()
	defer s.write.Unlock()

	wbuf, err := drpcenc.MarshalAppend(msg, enc, s.wbuf[:0])
	if err != nil {
		return errs.Wrap(err)
	}
	if s.opts.MaximumBufferSize == 0 || len(wbuf) < s.opts.MaximumBufferSize {
		s.wbuf = wbuf
	}
	if err := s.rawWriteLocked(drpcwire.KindMessage, wbuf); err != nil {
		return err
	}
	return nil
}

// MsgRecv recives some message data and unmarshals it with enc into msg.
func (s *Stream) MsgRecv(msg drpc.Message, enc drpc.Encoding) (err error) {
	defer func() { err = drpc.ToRPCErr(err) }()

	defer s.checkFinished()
	s.read.Lock()
	defer s.read.Unlock()

	b, err := s.recvQueue.Dequeue()
	if err != nil {
		return err
	}
	err = enc.Unmarshal(b, msg)
	s.recvQueue.Done()

	return err
}

//
// terminal messages
//

var (
	sendClosed     = drpc.Error.New("send closed")
	termError      = drpc.Error.New("stream terminated by sending error")
	termClosed     = drpc.Error.New("stream terminated by sending close")
	termBothClosed = drpc.Error.New("stream terminated by both issuing close send")
)

// SendError terminates the stream and sends the error to the remote. It is a
// no-op if the stream is already terminated.
//
// SendError is ordered: a message whose MsgSend returned before this call is
// delivered to the remote ahead of the error. This is unlike SendCancel.
func (s *Stream) SendError(serr error) (err error) {
	s.log("CALL", func() string { return fmt.Sprintf("SendError(%v)", serr) })

	s.mu.Lock()
	if s.sigs.term.IsSet() {
		s.mu.Unlock()
		return nil
	}

	defer s.checkFinished()
	s.write.Lock()
	defer s.write.Unlock()

	s.sigs.send.Set(io.EOF) // in this state, gRPC returns io.EOF on send.
	s.terminate(termError)
	s.mu.Unlock()

	return s.CheckCancelError(s.sendPacketLocked(drpcwire.KindError, false, drpcwire.MarshalError(serr)))
}

// SendCancel terminates the stream and sends a cancel to the remote side. It is
// a no-op if the stream is already terminated.
//
// SendCancel is abortive: unlike the ordered terminal sends it does not queue
// behind in-flight messages and gives no guarantee that already-sent messages
// reach the remote first. This matches gRPC, where cancellation does not flush
// buffered messages.
func (s *Stream) SendCancel(err error) error {
	s.log("CALL", func() string { return "SendCancel()" })

	s.mu.Lock()
	if s.sigs.term.IsSet() {
		s.mu.Unlock()
		return nil
	}

	defer s.checkFinished()

	// Set the send signal before taking the write lock so a send blocked on
	// backpressure is interrupted and releases the lock, letting the cancel
	// preempt it instead of waiting for the buffer to drain.
	s.sigs.send.Set(io.EOF) // in this state, gRPC returns io.EOF on send.
	s.terminate(err)
	s.mu.Unlock()

	s.write.Lock()
	defer s.write.Unlock()

	return s.CheckCancelError(s.sendPacketLocked(drpcwire.KindCancel, true, nil))
}

// Close terminates the stream and sends that the stream has been closed to the
// remote. It is a no-op if the stream is already terminated.
//
// Close is ordered: a message whose MsgSend returned before this call is
// delivered to the remote ahead of the close.
func (s *Stream) Close() (err error) {
	s.log("CALL", func() string { return "Close()" })

	s.mu.Lock()
	if s.sigs.term.IsSet() {
		s.mu.Unlock()
		return nil
	}

	defer s.checkFinished()
	s.write.Lock()
	defer s.write.Unlock()

	s.terminate(termClosed)
	s.mu.Unlock()

	return s.CheckCancelError(s.sendPacketLocked(drpcwire.KindClose, false, nil))
}

// CloseSend informs the remote that no more messages will be sent. If the remote has
// also already issued a CloseSend, the stream is terminated. It is a no-op if the
// stream already has sent a CloseSend or if it is terminated.
//
// CloseSend is ordered: a message whose MsgSend returned before this call is
// delivered to the remote ahead of the close-send.
func (s *Stream) CloseSend() (err error) {
	s.log("CALL", func() string { return "CloseSend()" })

	s.mu.Lock()
	if s.sigs.send.IsSet() || s.sigs.term.IsSet() {
		s.mu.Unlock()
		return nil
	}

	defer s.checkFinished()
	s.write.Lock()
	defer s.write.Unlock()

	s.sigs.send.Set(sendClosed)
	s.terminateIfBothClosed()
	s.mu.Unlock()

	return s.CheckCancelError(s.sendPacketLocked(drpcwire.KindCloseSend, false, nil))
}

// Cancel transitions the stream into a state where all writes to the transport will return
// the provided error, and terminates the stream. It is a no-op if the stream is already
// finished, and returns a boolean indicating if that was the case.
func (s *Stream) Cancel(err error) bool {
	s.log("CALL", func() string { return fmt.Sprintf("Cancel(%v)", err) })

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.IsFinished() {
		return true
	}

	s.sigs.cancel.Set(err)
	s.sigs.send.Set(io.EOF) // in this state, gRPC returns io.EOF on send.
	s.terminate(err)
	return false
}
