// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package integration

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/zeebo/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"storj.io/drpc/drpcconn"
	"storj.io/drpc/drpcmanager"
	"storj.io/drpc/drpcmux"
	"storj.io/drpc/drpcserver"
	"storj.io/drpc/drpctest"
)

func TestTransport_Error(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	// create a channel to signal when the rpc has started
	started := make(chan struct{})

	// create a server that signals then waits for the context to die
	cli, close := createConnection(t, impl{
		Method1Fn: func(ctx context.Context, _ *In) (*Out, error) {
			started <- struct{}{}
			<-ctx.Done()
			return nil, nil
		},
	})
	defer close()

	// async start the client issuing the rpc
	ctx.Run(func(ctx context.Context) { _, _ = cli.Method1(ctx, in(1)) })

	// wait for it to be started
	<-started

	// kill the transport from underneath of it
	assert.NoError(t, cli.DRPCConn().(*drpcconn.Conn).Transport().Close())
}

func TestTransport_Blocked(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	// create a channel to hold the rpc error
	errch := make(chan error, 1)

	// create a transport that signals when reads/writes happen
	trs := new(transportSignaler)
	defer func() { assert.NoError(t, trs.Close()) }()

	// start a client issuing an rpc that we keep track of
	cli := NewDRPCServiceClient(drpcconn.New(trs))
	ctx.Run(func(ctx context.Context) {
		_, err := cli.Method1(ctx, in(1))
		errch <- err
	})

	// wait for the write to happen before canceling the context. this
	// should cause the rpc goroutine to exit.
	<-trs.write.Signal()
	ctx.Cancel()

	// we should always get a canceled error from issuing the rpc: not
	// the error returned by the transport due to a read/write.
	err := <-errch
	st, ok := status.FromError(err)
	assert.That(t, ok)
	assert.Equal(t, st.Code(), codes.Canceled)
}

func TestTransport_ErrorCausesCancel(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	// create a channel to signal when the rpc has started
	started := make(chan struct{})
	serr := make(chan error, 1)
	cerr := make(chan error, 1)

	// create a server that signals then waits for the context to die
	cli, close := createConnection(t, impl{
		Method2Fn: func(stream DRPCService_Method2Stream) error {
			started <- struct{}{}
			serr <- stream.MsgRecv(nil, Encoding)
			return nil
		},
	})
	defer close()

	// async start the client issuing the rpc
	ctx.Run(func(ctx context.Context) {
		stream, _ := cli.Method2(ctx)
		started <- struct{}{}
		cerr <- stream.MsgRecv(nil, Encoding)
	})

	// wait for it to be started. it is important to wait for
	// both the client and server to be started, otherwise there's
	// a race due to the client performing multiple operations to
	// invoke, and the server can send on started before the client
	// returns the stream, causing the client to return <nil>, canceled.
	<-started
	<-started

	// kill the transport from underneath of it
	assert.NoError(t, cli.DRPCConn().(*drpcconn.Conn).Transport().Close())

	// the server should always be context.Canceled because it for sure sees
	// that the remote side closed the connection.
	{
		err := <-serr
		t.Log("server error:", err)
		st, ok := status.FromError(err)
		assert.That(t, ok)
		assert.Equal(t, st.Code(), codes.Canceled)
	}

	// net.Pipe has a nondeterministic select inside of the read call on the local
	// side and remote side being closed, and in some rare cases it will see the
	// remote side closed first, returning io.EOF instead of io.ErrClosedPipe, so
	// we have to check that as well. The error may be wrapped as a gRPC status
	// error with codes.Canceled or codes.Unavailable.
	{
		err := <-cerr
		t.Log("client error:", err)
		isExpectedError := errors.Is(err, io.ErrClosedPipe)
		if st, ok := status.FromError(err); ok {
			isExpectedError = isExpectedError || st.Code() == codes.Canceled || st.Code() == codes.Unavailable
		}
		assert.That(t, isExpectedError)
	}
}

// TestTransport_ClosedWhileHandlerBlockedBeforeRecv reproduces a deadlock
// where the server handler is doing work before calling Recv() while
// manageReader has already read a message from the transport and is blocked
// in packetBuffer.Put(). When the client closes the transport, manageReader
// cannot detect the closure because it is stuck in Put(), so the server
// stream's context is never canceled.
//
// This reproduces the issue seen in CockroachDB's TestReceiveSnapshotLogging
// "cancel during receive" subtest, where the snapshot receiver handler is
// blocked in BeforeRecvAcceptedSnapshot (a test knob) before calling
// MsgRecv(), and the delegate's cancellation closes the transport but the
// server never detects it.
func TestTransport_ClosedWhileHandlerBlockedBeforeRecv(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	// Test knobs — channels for synchronization, mirroring CockroachDB's
	// receiveStartedCh and svrContextDone pattern.
	handlerStarted := make(chan struct{})
	svrCtxDone := make(chan struct{})

	// Set up server and client manually to use SoftCancel: false on
	// the client, matching CockroachDB's configuration.
	c1, c2 := net.Pipe()
	defer func() { _ = c1.Close() }()
	defer func() { _ = c2.Close() }()

	mux := drpcmux.New()
	assert.NoError(t, DRPCRegisterService(mux, impl{
		Method2Fn: func(stream DRPCService_Method2Stream) error {
			// The handler has started but has work to do before
			// reading messages. In CockroachDB, this corresponds to
			// the snapshot receiver sending the ACCEPTED response
			// and hitting the BeforeRecvAcceptedSnapshot test knob
			// before calling MsgRecv().
			close(handlerStarted)

			// Block until the stream context is canceled. With the
			// deadlock bug, this never fires because manageReader
			// is stuck in packetBuffer.Put() and cannot detect the
			// transport closure.
			select {
			case <-stream.Context().Done():
				close(svrCtxDone)
				return stream.Context().Err()
			}
		},
	}))
	srv := drpcserver.New(mux)
	ctx.Run(func(ctx context.Context) { _ = srv.ServeOne(ctx, c1) })

	// Client connection with SoftCancel: false. When the client
	// context is canceled, manageStream calls stream.Cancel() and
	// then m.terminate() which closes the transport — the same
	// code path as CockroachDB's delegate cancellation.
	conn := drpcconn.NewWithOptions(c2, drpcconn.Options{
		Manager: drpcmanager.Options{SoftCancel: false},
	})
	defer func() { _ = conn.Close() }()

	// Create a cancelable context for the client RPC, simulating
	// the delegate's context that gets canceled when the test
	// calls cancel().
	rpcCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cli := NewDRPCServiceClient(conn)

	// Start a client-streaming RPC. NewStream buffers the invoke
	// packet but does not flush it — the flush happens on the first
	// Send() call.
	stream, err := cli.Method2(rpcCtx)
	assert.NoError(t, err)

	// Send a message. This flushes both the invoke and the message
	// in a single write. The server's manageReader reads the invoke
	// (which triggers NewServerStream → handleRPC → handler start)
	// and then the KindMessage (which enters packetBuffer.Put() and
	// blocks because the handler hasn't called Recv() yet).
	assert.NoError(t, stream.Send(in(1)))

	// Wait for the handler to start.
	<-handlerStarted

	// Allow manageReader time to enter packetBuffer.Put() after
	// delivering the invoke packet.
	time.Sleep(100 * time.Millisecond)

	// Cancel the client RPC context. This triggers:
	//   manageStream detects ctx.Done()
	//   → stream.Cancel(ctx.Err()) returns false (not finished)
	//   → m.terminate(ctx.Err())
	//   → m.tr.Close() closes the transport
	// This is the same code path as CockroachDB's delegate
	// cancellation closing the TCP connection to the receiver.
	cancel()

	// The server handler's stream context should be canceled.
	select {
	case <-svrCtxDone:
		// Transport closure propagated to the handler.
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: server handler's stream context was not " +
			"canceled after client transport closed; manageReader is " +
			"stuck in packetBuffer.Put()")
	}
}
