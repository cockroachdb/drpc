// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpc

import (
	"context"
	"io"

	"github.com/zeebo/errs"
)

// HTTPRoute describes an HTTP route for a DRPC gateway endpoint.
// Generated code emits functions that return slices of these, one per
// HTTP binding on a google.api.http-annotated RPC method (including
// additional_bindings, so a single method can produce multiple entries).
type HTTPRoute struct {
	Method  string
	Path    string
	Handler any
}

// These error classes cover the common errors that drpc generates. ToRPCErr
// knows how to translate them into gRPC status codes, so the rule is to classify
// an error where it happens: when a subsystem hits a terminal error, or gets one
// from a library it calls, it should tag the error with the class that matches
// the cause. ToRPCErr then turns that class into a gRPC code at the boundary.
//
// ConnectionError and ClosedError mean the connection is gone, either because
// the transport died or because we closed it on purpose. ToRPCErr maps both to
// codes.Unavailable.
//
// ProtocolError and InternalError are real faults, not connection problems. We
// leave these for ToRPCErr to report as codes.Unknown or codes.Internal, so an
// actual bug does not get hidden behind a retryable "connection lost" error.
var (
	Error           = errs.Class("drpc")
	InternalError   = errs.Class("internal error")
	ProtocolError   = errs.Class("protocol error")
	ConnectionError = errs.Class("connection error")
	ClosedError     = errs.Class("closed")
)

// Transport is an interface describing what is required for a drpc connection.
// Any net.Conn can be used as a Transport.
type Transport interface {
	io.Reader
	io.Writer
	io.Closer
}

// Message is a protobuf message. It is expected to be used with an Encoding.
// This exists so that one can use whatever protobuf library/runtime they want.
type Message interface{}

// Conn represents a client connection to a server.
type Conn interface {
	// Close closes the connection.
	Close() error

	// Closed returns a channel that is closed if the connection is definitely closed.
	Closed() <-chan struct{}

	// Invoke issues a unary RPC to the remote. Only one Invoke or Stream may be
	// open at once.
	Invoke(ctx context.Context, rpc string, enc Encoding, in, out Message) error

	// NewStream starts a stream with the remote. Only one Invoke or Stream may be
	// open at once.
	NewStream(ctx context.Context, rpc string, enc Encoding) (Stream, error)
}

// StreamKind represents the type of stream ("unknown", "cli", or "srv").
type StreamKind uint8

const (
	StreamKindUnknown StreamKind = iota
	StreamKindClient
	StreamKindServer
)

// String returns the string representation of the StreamKind.
func (k StreamKind) String() string {
	switch k {
	case StreamKindClient:
		return "cli"
	case StreamKindServer:
		return "srv"
	default:
		return "unknown"
	}
}

// Stream is a bi-directional stream of messages to some other party.
type Stream interface {
	// Context returns the context associated with the stream. It is canceled
	// when the Stream is closed and no more messages will ever be sent or
	// received on it.
	Context() context.Context

	// MsgSend sends the Message to the remote.
	MsgSend(msg Message, enc Encoding) error

	// MsgRecv receives a Message from the remote.
	MsgRecv(msg Message, enc Encoding) error

	// CloseSend signals to the remote that we will no longer send any messages.
	CloseSend() error

	// Close closes the stream.
	Close() error
}

// Receiver is invoked by a server for a given RPC.
type Receiver = func(srv interface{}, ctx context.Context, in1, in2 interface{}) (out Message, err error)

// Description is the interface implemented by things that can be registered by
// a Server.
type Description interface {
	// NumMethods returns the number of methods available.
	NumMethods() int

	// Method returns the information about the nth method along with a handler
	// to invoke it. The method interface that it returns is expected to be
	// a method expression like `(*Type).HandlerName`.
	Method(n int) (rpc string, encoding Encoding, receiver Receiver, method interface{}, ok bool)
}

// Mux is a type that can have an implementation and a Description registered with it.
type Mux interface {
	// Register marks that the description should dispatch RPCs that it describes to
	// the provided srv.
	Register(srv interface{}, desc Description) error
}

// Handler handles streams and RPCs dispatched to it by a Server.
type Handler interface {
	// HandleRPC executes the RPC identified by the rpc string using the stream to
	// communicate with the remote.
	HandleRPC(stream Stream, rpc string) (err error)
}

// Encoding represents a way to marshal/unmarshal Message types.
type Encoding interface {
	// Marshal returns the encoded form of msg.
	Marshal(msg Message) ([]byte, error)

	// Unmarshal reads the encoded form of some Message into msg.
	// The buf is expected to contain only a single complete Message.
	Unmarshal(buf []byte, msg Message) error
}
