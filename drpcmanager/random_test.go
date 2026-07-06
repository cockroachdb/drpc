// Copyright (C) 2021 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcmanager

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/zeebo/assert"
	"google.golang.org/grpc/status"

	"storj.io/drpc"
	"storj.io/drpc/drpcstream"
	"storj.io/drpc/drpctest"
	"storj.io/drpc/drpcwire"
)

func TestRandomized_Client(t *testing.T) {
	t.Skip("disabled as the generated random workload violates the wire protocol")
	runRandomized(t, randomBytes(time.Now().UnixNano(), 1024), new(randClient))
}

func TestRandomized_Server(t *testing.T) {
	t.Skip("disabled as the generated random workload violates the wire protocol")
	runRandomized(t, randomBytes(time.Now().UnixNano(), 1024), new(randServer))
}

//
// client tests
//

type randClient struct {
	id     incID
	active bool
}

func (rc *randClient) newSteam(ctx context.Context, man *Manager) (*drpcstream.Stream, error) {
	stream, _, err := man.NewServerStream(ctx)
	return stream, err
}

func (rc *randClient) execute(t *testing.T, wr *drpcwire.MuxWriter, op byte) {
	cmd, arg, done := parseOp(op)

	if !rc.active {
		assert.NoError(t, wr.WriteFrame(drpcwire.Frame{
			Data: make([]byte, arg),
			ID:   rc.id.incMessage(),
			Kind: drpcwire.KindInvoke,
			Done: true,
		}, nil))
		rc.active = true
	}

	switch cmd {
	case 0: // new invoke
		if rc.active {
			assert.NoError(t, wr.WriteFrame(drpcwire.Frame{
				ID:   rc.id.incMessage(),
				Kind: drpcwire.KindClose,
				Done: true,
			}, nil))
		}

		rc.id.incStream()

		for i := 0; i < arg; i++ {
			assert.NoError(t, wr.WriteFrame(drpcwire.Frame{
				ID:   rc.id.incMessage(),
				Kind: drpcwire.KindInvokeMetadata,
				Done: done,
			}, nil))
		}

		assert.NoError(t, wr.WriteFrame(drpcwire.Frame{
			Data: make([]byte, arg),
			ID:   rc.id.incMessage(),
			Kind: drpcwire.KindInvoke,
			Done: done,
		}, nil))
		rc.active = done

	case 1: // terminate (close send, close, error)
		kind := [...]drpcwire.Kind{
			drpcwire.KindCloseSend,
			drpcwire.KindClose,
			drpcwire.KindError,
		}[arg%3]

		assert.NoError(t, wr.WriteFrame(drpcwire.Frame{
			Data: make([]byte, 8),
			ID:   rc.id.incMessage(),
			Kind: kind,
			Done: done,
		}, nil))

	case 2: // cause the remote side to close
		assert.NoError(t, wr.WriteFrame(drpcwire.Frame{
			Data: []byte("remote-close"),
			ID:   rc.id.incMessage(),
			Kind: drpcwire.KindMessage,
			Done: true,
		}, nil))

	case 3, 4, 5, 6, 7: // send normal message
		assert.NoError(t, wr.WriteFrame(drpcwire.Frame{
			Data: make([]byte, arg),
			ID:   rc.id.incMessage(),
			Kind: drpcwire.KindMessage,
			Done: done,
		}, nil))

	default:
		t.Fatalf("unknown command: %d", cmd)
	}
}

//
// server tests
//

type randServer struct {
	id incID
}

func (rs *randServer) newSteam(ctx context.Context, man *Manager) (*drpcstream.Stream, error) {
	return man.NewClientStream(ctx, "rpc")
}

func (rs *randServer) execute(t *testing.T, wr *drpcwire.MuxWriter, op byte) {
	cmd, arg, done := parseOp(op)

	switch cmd {
	case 0: // begin a new stream
		rs.id.incStream()

		assert.NoError(t, wr.WriteFrame(drpcwire.Frame{
			Data: make([]byte, arg),
			ID:   rs.id.incMessage(),
			Kind: drpcwire.KindMessage,
			Done: done,
		}, nil))

	case 1: // terminate (close send, close, error)
		kind := [...]drpcwire.Kind{
			drpcwire.KindCloseSend,
			drpcwire.KindClose,
			drpcwire.KindError,
			drpcwire.KindCancel,
		}[arg%4]

		assert.NoError(t, wr.WriteFrame(drpcwire.Frame{
			Data: make([]byte, 8),
			ID:   rs.id.incMessage(),
			Kind: kind,
			Done: done,
		}, nil))

	case 2: // cause the remote side to close
		assert.NoError(t, wr.WriteFrame(drpcwire.Frame{
			Data: []byte("remote-close"),
			ID:   rs.id.incMessage(),
			Kind: drpcwire.KindMessage,
			Done: true,
		}, nil))

	case 3, 4, 5, 6, 7: // send random message
		assert.NoError(t, wr.WriteFrame(drpcwire.Frame{
			Data: make([]byte, arg),
			ID:   rs.id.incMessage(),
			Kind: drpcwire.KindMessage,
			Done: done,
		}, nil))

	default:
		t.Fatalf("unknown command: %d", cmd)
	}
}

//
// test runner
//

type runner interface {
	newSteam(ctx context.Context, man *Manager) (*drpcstream.Stream, error)
	execute(t *testing.T, wr *drpcwire.MuxWriter, op byte)
}

func runRandomized(t *testing.T, prog []byte, r runner) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	pc, ps := net.Pipe()
	defer func() { _ = pc.Close() }()
	defer func() { _ = ps.Close() }()

	wr := drpcwire.NewMuxWriter(pc, func(error) {})
	defer func() { wr.Stop(nil); <-wr.Done() }()

	man := New(ps, Server)
	defer func() { _ = man.Close() }()

	errch := make(chan error, 1)
	ctx.Run(func(ctx context.Context) {
		errch <- func() error {
			for {
				stream, err := r.newSteam(ctx, man)
				if err != nil {
					return err
				}
				for {
					buf, err := stream.RawRecv()
					if expectedError(err) || string(buf) == "remote-close" {
						stream.Cancel(context.Canceled)
						break
					} else if err != nil {
						return err
					}
				}
			}
		}()
	})

	for _, op := range prog {
		r.execute(t, wr, op)
	}

	assert.NoError(t, man.Close())
	// A deliberate Close reaches consumers as a ClosedError, which drpc.ToRPCErr
	// maps to codes.Unavailable, and it still keeps the original "manager closed:
	// Close called" cause in the chain.
	err := <-errch
	assert.That(t, drpc.ClosedError.Has(err))
	assert.That(t, strings.Contains(err.Error(), "manager closed: Close called"))
}

//
// helpers
//

func expectedError(err error) bool {
	if err == nil {
		return false
	}
	// Standard expected errors
	if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return true
	}
	// Legacy format: empty error message
	if err.Error() == "" {
		return true
	}
	// gRPC status errors are expected when random data happens to parse as valid protobuf
	if _, ok := status.FromError(err); ok {
		return true
	}
	// Wire format errors are expected when random data is sent as error frames
	errMsg := err.Error()
	return strings.HasPrefix(errMsg, "drpcwire:")
}

func parseOp(op byte) (cmd byte, arg int, done bool) {
	cmd, op = op&0b111, op>>3
	arg, op = int(op&0b1111), op>>4
	done = op&0b1 > 0
	return cmd, arg, done
}

func randomBytes(seed int64, n int) []byte {
	out := make([]byte, n)
	_, _ = rand.New(rand.NewSource(seed)).Read(out)
	return out
}

type incID drpcwire.ID

func (id *incID) incStream() { *id = incID{Stream: id.Stream + 1} }
func (id *incID) incMessage() drpcwire.ID {
	id.Message++
	return drpcwire.ID{Stream: id.Stream + 1, Message: id.Message}
}
