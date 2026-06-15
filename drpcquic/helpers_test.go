// Copyright (C) 2026 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcquic

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/stretchr/testify/require"
	"storj.io/drpc"
)

// testTLS returns a (server, client) tls.Config pair. The server presents a
// fresh self-signed cert; the client skips verification (test only) and
// advertises the drpcquic ALPN.
func testTLS(t testing.TB) (server, client *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "drpcquic-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}

	server = &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{ALPN}}
	client = &tls.Config{InsecureSkipVerify: true, NextProtos: []string{ALPN}} //nolint:gosec // test only
	return server, client
}

// newStreamPair establishes a loopback QUIC connection and returns the client's
// opened stream plus an acceptServer function that returns the matching
// server-side stream. acceptServer must be called AFTER the first client write,
// because QUIC creates the server-side stream lazily on first data.
func newStreamPair(t *testing.T) (client *quic.Stream, acceptServer func() *quic.Stream, cleanup func()) {
	t.Helper()
	serverTLS, clientTLS := testTLS(t)
	ln, err := quic.ListenAddr("127.0.0.1:0", serverTLS, nil)
	require.NoError(t, err)
	ctx := context.Background()

	sconnCh := make(chan *quic.Conn, 1)
	go func() {
		c, err := ln.Accept(ctx)
		if err != nil {
			sconnCh <- nil
			return
		}
		sconnCh <- c
	}()

	cconn, err := quic.DialAddr(ctx, ln.Addr().String(), clientTLS, nil)
	require.NoError(t, err)
	cs, err := cconn.OpenStreamSync(ctx)
	require.NoError(t, err)

	sconn := <-sconnCh
	require.NotNil(t, sconn)

	acceptServer = func() *quic.Stream {
		ss, err := sconn.AcceptStream(ctx)
		require.NoError(t, err)
		return ss
	}
	cleanup = func() {
		_ = cconn.CloseWithError(0, "")
		_ = sconn.CloseWithError(0, "")
		_ = ln.Close()
	}
	return cs, acceptServer, cleanup
}

// strMsg is a trivial drpc.Message carrying a string, for tests.
type strMsg struct{ S string }

// strEnc is a trivial drpc.Encoding for strMsg.
type strEnc struct{}

func (strEnc) Marshal(m drpc.Message) ([]byte, error) { return []byte(m.(*strMsg).S), nil }
func (strEnc) Unmarshal(b []byte, m drpc.Message) error {
	m.(*strMsg).S = string(b)
	return nil
}

// echoHandler implements drpc.Handler: "/echo" is unary (echo one message),
// "/stream" echoes each received message until the client closes its send side.
type echoHandler struct{}

func (echoHandler) HandleRPC(stream drpc.Stream, rpc string) error {
	switch rpc {
	case "/echo":
		in := new(strMsg)
		if err := stream.MsgRecv(in, strEnc{}); err != nil {
			return err
		}
		return stream.MsgSend(&strMsg{S: "echo:" + in.S}, strEnc{})
	case "/stream":
		for {
			in := new(strMsg)
			if err := stream.MsgRecv(in, strEnc{}); err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
			if err := stream.MsgSend(&strMsg{S: "echo:" + in.S}, strEnc{}); err != nil {
				return err
			}
		}
	}
	return drpc.ProtocolError.New("unknown rpc %q", rpc)
}

// NOTE: startServer (which needs Listen/Serve) is defined in server_test.go in
// Phase 5, so this helpers file compiles before the server exists.

func TestHarness_StreamPairRoundTrip(t *testing.T) {
	client, acceptServer, cleanup := newStreamPair(t)
	defer cleanup()

	_, err := client.Write([]byte("ping"))
	require.NoError(t, err)

	server := acceptServer()
	buf := make([]byte, 4)
	_, err = io.ReadFull(server, buf)
	require.NoError(t, err)
	require.Equal(t, "ping", string(buf))
}
