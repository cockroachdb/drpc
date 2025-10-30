// Copyright (C) 2023 Elara Musayelyan
// Copyright (C) 2025 Cockroach Labs
// See LICENSE for copying information.

package drpcyamux

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/hashicorp/yamux"
	"storj.io/drpc"
	"storj.io/drpc/drpcconn"
)

var ErrClosed = errors.New("connection closed")

var _ drpc.Conn = &Conn{}

// Conn implements drpc.Conn using the yamux multiplexer to allow concurrent
// RPCs
type Conn struct {
	conn io.ReadWriteCloser
	sess *yamux.Session

	closeOnce sync.Once
	closeErr  error
	closed    chan struct{}
}

// NewConn returns a new multiplexed DRPC connection as a client
func NewConn(conn io.ReadWriteCloser) (*Conn, error) {
	return NewConnWithConfig(conn, nil)
}

// NewConnWithConfig returns a new multiplexed DRPC connection as a client
// with the given yamux configuration
func NewConnWithConfig(conn io.ReadWriteCloser, config *yamux.Config) (*Conn, error) {
	sess, err := yamux.Client(conn, config)
	if err != nil {
		return nil, err
	}

	return &Conn{
		conn:   conn,
		sess:   sess,
		closed: make(chan struct{}),
	}, nil
}

// Close closes the multiplexer session and the underlying connection. It is
// safe to call Close multiple times.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)

		// Close session first to stop accepting new streams
		sessErr := c.sess.Close()

		// Always close the underlying connection
		connErr := c.conn.Close()

		// Return the first error encountered
		if sessErr != nil {
			c.closeErr = sessErr
		} else {
			c.closeErr = connErr
		}
	})
	return c.closeErr
}

// Closed returns a channel that will be closed
// when the connection is closed
func (c *Conn) Closed() <-chan struct{} {
	return c.closed
}

// Invoke issues the rpc on the transport serializing in, waits for a response,
// and deserializes it into out.
func (c *Conn) Invoke(
	ctx context.Context, rpc string, enc drpc.Encoding, in, out drpc.Message,
) error {
	select {
	case <-c.closed:
		return ErrClosed
	default:
	}

	stream, err := c.sess.Open()
	if err != nil {
		return err
	}
	defer stream.Close()

	dconn := drpcconn.New(stream)
	defer dconn.Close()

	return dconn.Invoke(ctx, rpc, enc, in, out)
}

// NewStream begins a streaming rpc on the connection.
func (c *Conn) NewStream(ctx context.Context, rpc string, enc drpc.Encoding) (drpc.Stream, error) {
	select {
	case <-c.closed:
		return nil, ErrClosed
	default:
	}

	stream, err := c.sess.Open()
	if err != nil {
		return nil, err
	}

	dconn := drpcconn.New(stream)

	s, err := dconn.NewStream(ctx, rpc, enc)
	if err != nil {
		dconn.Close()
		stream.Close()
		return nil, err
	}

	// Clean up the yamux stream when the drpc connection closes.
	// This goroutine will exit when dconn.Closed() is signaled.
	go func() {
		<-dconn.Closed()
		stream.Close()
	}()

	return s, nil
}
