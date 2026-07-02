// Copyright (C) 2024 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcquic_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeebo/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"storj.io/drpc"
	"storj.io/drpc/drpcmanager"
	"storj.io/drpc/drpcmetadata"
	"storj.io/drpc/drpcquic"
	"storj.io/drpc/drpcserver"
	"storj.io/drpc/drpctest"
	"storj.io/drpc/drpcwire"
)

// protoEncoding is the DRPC (protobuf) encoding, exactly like generated code.
type protoEncoding struct{}

func (protoEncoding) Marshal(msg drpc.Message) ([]byte, error) {
	return proto.Marshal(msg.(proto.Message))
}

func (protoEncoding) Unmarshal(buf []byte, msg drpc.Message) error {
	return proto.Unmarshal(buf, msg.(proto.Message))
}

var enc = protoEncoding{}

// testHandler is a hand-rolled drpc.Handler that dispatches on the rpc name.
type testHandler struct{}

func (testHandler) HandleRPC(stream drpc.Stream, rpc string) error {
	switch rpc {
	case "/test/Echo":
		in := new(wrapperspb.StringValue)
		if err := stream.MsgRecv(in, enc); err != nil {
			return err
		}
		return stream.MsgSend(in, enc)

	case "/test/Count":
		in := new(wrapperspb.Int64Value)
		if err := stream.MsgRecv(in, enc); err != nil {
			return err
		}
		for i := int64(0); i < in.Value; i++ {
			if err := stream.MsgSend(wrapperspb.Int64(i), enc); err != nil {
				return err
			}
		}
		return nil

	case "/test/Fail":
		in := new(wrapperspb.StringValue)
		if err := stream.MsgRecv(in, enc); err != nil {
			return err
		}
		return status.Error(codes.FailedPrecondition, "boom")

	case "/test/Meta":
		in := new(wrapperspb.StringValue)
		if err := stream.MsgRecv(in, enc); err != nil {
			return err
		}
		md, _ := drpcmetadata.GetFromIncomingContext(stream.Context())
		return stream.MsgSend(wrapperspb.String(md["key"]), enc)

	default:
		return drpc.ProtocolError.New("unknown rpc %q", rpc)
	}
}

// genTLS returns matched server and client TLS configs using a freshly
// generated self-signed ECDSA certificate.
func genTLS(t *testing.T) (server, client *tls.Config) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	assert.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	assert.NoError(t, err)

	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
	server = &tls.Config{Certificates: []tls.Certificate{cert}}
	client = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test only
	return server, client
}

// handlerFunc adapts a function to the drpc.Handler interface.
type handlerFunc func(stream drpc.Stream, rpc string) error

func (f handlerFunc) HandleRPC(stream drpc.Stream, rpc string) error { return f(stream, rpc) }

// setup stands up a QUIC server on loopback and returns a connected client.
func setup(t *testing.T, ctx *drpctest.Tracker, handler drpc.Handler) *drpcquic.QuicConn {
	serverTLS, clientTLS := genTLS(t)

	lis, err := drpcquic.Listen("127.0.0.1:0", serverTLS)
	assert.NoError(t, err)

	srv := drpcserver.New(handler)
	ctx.Run(func(ctx context.Context) { _ = srv.ServeQuic(ctx, lis) })

	conn, err := drpcquic.Dial(ctx, lis.Addr().String(), clientTLS)
	assert.NoError(t, err)
	return conn
}

func TestQuic_Unary(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	conn := setup(t, ctx, testHandler{})
	defer func() { _ = conn.Close() }()

	out := new(wrapperspb.StringValue)
	err := conn.Invoke(ctx, "/test/Echo", enc, wrapperspb.String("hello"), out)
	assert.NoError(t, err)
	assert.Equal(t, out.Value, "hello")
}

func TestQuic_ServerStream(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	conn := setup(t, ctx, testHandler{})
	defer func() { _ = conn.Close() }()

	stream, err := conn.NewStream(ctx, "/test/Count", enc)
	assert.NoError(t, err)
	assert.NoError(t, stream.MsgSend(wrapperspb.Int64(5), enc))
	assert.NoError(t, stream.CloseSend())

	var got []int64
	for {
		m := new(wrapperspb.Int64Value)
		err := stream.MsgRecv(m, enc)
		if errors.Is(err, io.EOF) {
			break
		}
		assert.NoError(t, err)
		got = append(got, m.Value)
	}
	assert.Equal(t, len(got), 5)
}

func TestQuic_ErrorCodeSurvives(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	conn := setup(t, ctx, testHandler{})
	defer func() { _ = conn.Close() }()

	out := new(wrapperspb.StringValue)
	err := conn.Invoke(ctx, "/test/Fail", enc, wrapperspb.String("x"), out)
	assert.Error(t, err)

	st, ok := status.FromError(err)
	assert.That(t, ok)
	assert.Equal(t, st.Code(), codes.FailedPrecondition)
	assert.Equal(t, st.Message(), "boom")
}

func TestQuic_Metadata(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	conn := setup(t, ctx, testHandler{})
	defer func() { _ = conn.Close() }()

	octx := drpcmetadata.AppendToOutgoingContext(ctx, map[string]string{"key": "val"})
	out := new(wrapperspb.StringValue)
	err := conn.Invoke(octx, "/test/Meta", enc, wrapperspb.String("x"), out)
	assert.NoError(t, err)
	assert.Equal(t, out.Value, "val")
}

func TestQuic_ConcurrentStreams(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	conn := setup(t, ctx, testHandler{})
	defer func() { _ = conn.Close() }()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			want := fmt.Sprint(i)
			out := new(wrapperspb.StringValue)
			err := conn.Invoke(ctx, "/test/Echo", enc, wrapperspb.String(want), out)
			assert.NoError(t, err)
			assert.Equal(t, out.Value, want)
		}(i)
	}
	wg.Wait()
}

func TestQuic_ServerObservesClientCancel(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	observed := make(chan error, 1)
	handler := handlerFunc(func(stream drpc.Stream, rpc string) error {
		in := new(wrapperspb.StringValue)
		if err := stream.MsgRecv(in, enc); err != nil {
			observed <- err
			return err
		}
		// Ack so the client knows the handler is now blocked on the stream
		// context, making the cancellation race-free.
		if err := stream.MsgSend(wrapperspb.String("ack"), enc); err != nil {
			observed <- err
			return err
		}
		<-stream.Context().Done()
		err := stream.Context().Err()
		observed <- err
		return err
	})

	conn := setup(t, ctx, handler)
	defer func() { _ = conn.Close() }()

	cctx, cancel := context.WithCancel(ctx)
	stream, err := conn.NewStream(cctx, "/test/Hang", enc)
	assert.NoError(t, err)
	assert.NoError(t, stream.MsgSend(wrapperspb.String("hi"), enc))

	ack := new(wrapperspb.StringValue)
	assert.NoError(t, stream.MsgRecv(ack, enc))
	assert.Equal(t, ack.Value, "ack")

	// Cancel the client's context; the server handler should unblock.
	cancel()

	select {
	case err := <-observed:
		assert.Error(t, err)
		assert.That(t, errors.Is(err, context.Canceled))
	case <-time.After(10 * time.Second):
		t.Fatal("server handler did not observe client cancellation")
	}
}

// TestQuic_LargeMessage exercises a message larger than the default 4 MiB cap,
// verifying that a raised MaxMessageSize (client) / Manager buffer (server)
// round-trips it — the path CockroachDB relies on for large RPCs like AddSSTable.
func TestQuic_LargeMessage(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	serverTLS, clientTLS := genTLS(t)

	lis, err := drpcquic.Listen("127.0.0.1:0", serverTLS)
	assert.NoError(t, err)

	const maxMsg = 16 << 20 // 16 MiB
	srv := drpcserver.NewWithOptions(testHandler{}, drpcserver.Options{
		Manager: drpcmanager.Options{
			Reader: drpcwire.ReaderOptions{MaximumBufferSize: maxMsg},
		},
	})
	ctx.Run(func(ctx context.Context) { _ = srv.ServeQuic(ctx, lis) })

	conn, err := drpcquic.DialWithOptions(ctx, lis.Addr().String(), clientTLS,
		drpcquic.Options{MaxMessageSize: maxMsg})
	assert.NoError(t, err)
	defer func() { _ = conn.Close() }()

	big := strings.Repeat("x", 8<<20) // 8 MiB, well over the 4 MiB default cap
	out := new(wrapperspb.StringValue)
	err = conn.Invoke(ctx, "/test/Echo", enc, wrapperspb.String(big), out)
	assert.NoError(t, err)
	assert.Equal(t, len(out.Value), len(big))
}
