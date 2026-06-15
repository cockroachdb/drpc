// Copyright (C) 2026 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcmanager

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"storj.io/drpc"
	"storj.io/drpc/drpcmetadata"
	"storj.io/drpc/drpcstream"
	"storj.io/drpc/drpcwire"
)

// rawEnc is a trivial []byte encoding for tests.
type rawEnc struct{}

func (rawEnc) Marshal(m drpc.Message) ([]byte, error) { return m.([]byte), nil }
func (rawEnc) Unmarshal(b []byte, m drpc.Message) error {
	p := m.(*[]byte)
	*p = append((*p)[:0], b...)
	return nil
}

// connPair returns a connected pair of net.Conn over loopback TCP (async,
// unlike net.Pipe).
func connPair() (net.Conn, net.Conn, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = ln.Close() }()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()
	a, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		return nil, nil, err
	}
	r := <-ch
	if r.err != nil {
		_ = a.Close()
		return nil, nil, r.err
	}
	return a, r.c, nil
}

// fakeMux is an in-memory drpc.MultiplexedTransport. OpenStream creates a fresh
// connected pair, returns one end, and delivers the other end to a peer's
// AcceptStream — emulating QUIC's independent per-stream connections.
type fakeMux struct {
	ch     chan drpc.Transport
	closed chan struct{}
	once   sync.Once
}

var _ drpc.MultiplexedTransport = (*fakeMux)(nil)

func newFakeMux() *fakeMux {
	return &fakeMux{ch: make(chan drpc.Transport), closed: make(chan struct{})}
}

func (f *fakeMux) OpenStream(ctx context.Context) (drpc.Transport, error) {
	a, b, err := connPair()
	if err != nil {
		return nil, err
	}
	select {
	case f.ch <- b:
		return a, nil
	case <-ctx.Done():
		_, _ = a.Close(), b.Close()
		return nil, ctx.Err()
	case <-f.closed:
		_, _ = a.Close(), b.Close()
		return nil, net.ErrClosed
	}
}

func (f *fakeMux) AcceptStream(ctx context.Context) (drpc.Transport, error) {
	select {
	case tr := <-f.ch:
		return tr, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.closed:
		return nil, net.ErrClosed
	}
}

func (f *fakeMux) Close() error             { f.once.Do(func() { close(f.closed) }); return nil }
func (f *fakeMux) Closed() <-chan struct{}  { return f.closed }

// A full invoke + message round-trip through client and server QUICManagers,
// validating sid==1 and that the same reader is threaded from invoke-read into
// the server stream's read loop (the message arrives after the invoke).
func TestQUICManager_RoundTrip(t *testing.T) {
	mux := newFakeMux()
	defer func() { _ = mux.Close() }()
	ctx := context.Background()

	cm := NewQUIC(mux, Options{})
	sm := NewQUIC(mux, Options{})

	type sres struct {
		st  *drpcstream.Stream
		rpc string
		err error
	}
	sch := make(chan sres, 1)
	go func() {
		st, rpc, err := sm.NewServerStream(ctx)
		sch <- sres{st, rpc, err}
	}()

	cs, err := cm.NewClientStream(ctx, "/echo")
	require.NoError(t, err)
	require.Equal(t, uint64(1), cs.ID()) // vestigial stream id is 1

	require.NoError(t, cs.RawWrite(drpcwire.KindInvoke, []byte("/echo")))
	require.NoError(t, cs.MsgSend([]byte("ping"), rawEnc{})) // also flushes the invoke
	require.NoError(t, cs.CloseSend())

	sr := <-sch
	require.NoError(t, sr.err)
	require.Equal(t, "/echo", sr.rpc)
	require.Equal(t, uint64(1), sr.st.ID())

	var req []byte
	require.NoError(t, sr.st.MsgRecv(&req, rawEnc{}))
	require.Equal(t, "ping", string(req))

	require.NoError(t, sr.st.MsgSend([]byte("pong"), rawEnc{}))
	require.NoError(t, sr.st.CloseSend())

	var resp []byte
	require.NoError(t, cs.MsgRecv(&resp, rawEnc{}))
	require.Equal(t, "pong", string(resp))

	_ = cs.Close()
	_ = sr.st.Close()
}

// Metadata sent before the invoke is decoded into the server stream's context.
func TestQUICManager_Metadata(t *testing.T) {
	mux := newFakeMux()
	defer func() { _ = mux.Close() }()
	ctx := context.Background()

	cm := NewQUIC(mux, Options{})
	sm := NewQUIC(mux, Options{})

	type sres struct {
		md  map[string]string
		ok  bool
		err error
	}
	sch := make(chan sres, 1)
	go func() {
		st, _, err := sm.NewServerStream(ctx)
		if err != nil {
			sch <- sres{err: err}
			return
		}
		md, ok := drpcmetadata.GetFromIncomingContext(st.Context())
		sch <- sres{md: md, ok: ok}
		var x []byte
		_ = st.MsgRecv(&x, rawEnc{})
		_ = st.Close()
	}()

	enc, err := drpcmetadata.Encode(nil, map[string]string{"hello": "world"})
	require.NoError(t, err)

	cs, err := cm.NewClientStream(ctx, "/echo")
	require.NoError(t, err)
	require.NoError(t, cs.RawWrite(drpcwire.KindInvokeMetadata, enc))
	require.NoError(t, cs.RawWrite(drpcwire.KindInvoke, []byte("/echo")))
	require.NoError(t, cs.MsgSend([]byte("x"), rawEnc{})) // flush invoke+metadata
	require.NoError(t, cs.CloseSend())

	sr := <-sch
	require.NoError(t, sr.err)
	require.True(t, sr.ok)
	require.Equal(t, "world", sr.md["hello"])

	_ = cs.Close()
}

func TestQUICManager_Unblocked(t *testing.T) {
	m := NewQUIC(newFakeMux(), Options{})
	select {
	case <-m.Unblocked(): // always already closed in multiplexed mode
	default:
		t.Fatal("Unblocked() should be already closed")
	}
}

func TestQUICManager_Closed(t *testing.T) {
	m := NewQUIC(newFakeMux(), Options{})
	select {
	case <-m.Closed():
		t.Fatal("should not be closed yet")
	default:
	}
	require.NoError(t, m.Close())
	select {
	case <-m.Closed():
	case <-time.After(time.Second):
		t.Fatal("Closed() not signaled after Close")
	}
}
