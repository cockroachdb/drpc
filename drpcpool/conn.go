// Copyright (C) 2022 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcpool

import (
	"context"

	"storj.io/drpc"
	"storj.io/drpc/drpcsignal"
)

// Conn is the type of connections that can be managed by the pool.
type Conn interface {
	drpc.Conn

	// Unblocked returns a channel that is closed when the conn is available
	// for an Invoke or NewStream call.
	Unblocked() <-chan struct{}
}

// poolConn is a wrapper that asks a Pool for an underlying conn when necessary.
type poolConn[K comparable] struct {
	done drpcsignal.Chan
	key  K
	pool *Pool[K]
	dial func(context.Context, K) (Conn, error)
}

// Close sets the poolConn to be in a closed state, inhibiting subsequent
// Invoke or NewStream calls.
func (p *poolConn[K]) Close() error {
	p.done.Close()
	return nil
}

// Closed returns a channel that is closed after calls to Invoke and NewStream
// are inhibited.
func (p *poolConn[K]) Closed() <-chan struct{} {
	return p.done.Get()
}

// Unblocked returns a channel that is closed when calls to Invoke and NewStream
// are not inhibited by a previous cancel. For this conn, previous cancels are
// always internally handled by the pool, so it is always unblocked.
func (p *poolConn[K]) Unblocked() <-chan struct{} { return closedCh }

// acquireConn tries the pool first, dials on miss, and inserts the new
// connection eagerly so concurrent callers can share it.
func (p *poolConn[K]) acquireConn(ctx context.Context) (*connState[K], error) {
	cs, ok := p.pool.acquire(p.key)
	if ok {
		return cs, nil
	}

	conn, err := p.dial(ctx, p.key)
	if err != nil {
		return nil, err
	}

	return p.pool.insertAndAcquire(p.key, conn), nil
}

// Invoke grabs a connection from the Pool (or dials a new one), calls Invoke,
// and releases the connection back.
func (p *poolConn[K]) Invoke(ctx context.Context, rpc string, enc drpc.Encoding, in drpc.Message, out drpc.Message) (err error) {
	defer func() { err = drpc.ToRPCErr(err) }()

	if closed(p.done.Get()) {
		return drpc.ClosedError.New("connection closed")
	}

	cs, err := p.acquireConn(ctx)
	if err != nil {
		return err
	}
	defer p.pool.release(cs)

	return cs.val.Invoke(ctx, rpc, enc, in, out)
}

// NewStream grabs a connection from the Pool (or dials a new one), calls
// NewStream, and sets up a goroutine to release the connection when the
// stream finishes.
func (p *poolConn[K]) NewStream(ctx context.Context, rpc string, enc drpc.Encoding) (_ drpc.Stream, err error) {
	defer func() { err = drpc.ToRPCErr(err) }()

	if closed(p.done.Get()) {
		return nil, drpc.ClosedError.New("connection closed")
	}

	cs, err := p.acquireConn(ctx)
	if err != nil {
		return nil, err
	}

	stream, err := cs.val.NewStream(ctx, rpc, enc)
	if err != nil {
		p.pool.release(cs)
		return nil, err
	}

	sw := &streamWrapper{
		Stream: stream,
		ctx:    streamWrapperContext{Context: ctx},
	}
	go p.monitorStream(stream, cs, &sw.ctx.done)

	return sw, nil
}

func (p *poolConn[K]) monitorStream(stream drpc.Stream, cs *connState[K], done *drpcsignal.Chan) {
	<-stream.Context().Done()
	p.pool.release(cs)
	done.Close()
}

type streamWrapper struct {
	drpc.Stream
	ctx streamWrapperContext
}

func (s *streamWrapper) Context() context.Context { return &s.ctx }

type streamWrapperContext struct {
	context.Context
	done drpcsignal.Chan
}

func (s *streamWrapperContext) Done() <-chan struct{} { return s.done.Get() }
