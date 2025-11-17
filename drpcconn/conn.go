// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcconn

import (
	"context"
	"fmt"
	"runtime/pprof"
	"sync"

	"github.com/google/uuid"
	"github.com/zeebo/errs"

	"storj.io/drpc"
	"storj.io/drpc/drpcenc"
	"storj.io/drpc/drpcmanager"
	"storj.io/drpc/drpcmetadata"
	"storj.io/drpc/drpcstats"
	"storj.io/drpc/drpcstream"
	"storj.io/drpc/drpcwire"
	"storj.io/drpc/internal/drpcopts"
)

// Options controls configuration settings for a conn.
type Options struct {
	// Manager controls the options we pass to the manager of this conn.
	Manager drpcmanager.Options

	// CollectStats controls whether the server should collect stats on the
	// rpcs it creates.
	CollectStats bool
}

// Conn is a drpc client connection.
type Conn struct {
	tr   drpc.Transport
	id   string
	man  *drpcmanager.Manager
	mu   sync.Mutex
	wbuf []byte

	stats map[string]*drpcstats.Stats
}

var _ drpc.Conn = (*Conn)(nil)

// New returns a conn that uses the transport for reads and writes.
func New(tr drpc.Transport) *Conn { return NewWithOptions(tr, Options{}) }

// NewWithOptions returns a conn that uses the transport for reads and writes.
// The Options control details of how the conn operates.
func NewWithOptions(tr drpc.Transport, opts Options) *Conn {
	c := &Conn{
		tr: tr,
		id: uuid.New().String(),
	}

	if opts.CollectStats {
		drpcopts.SetManagerStatsCB(&opts.Manager.Internal, c.getStats)
		c.stats = make(map[string]*drpcstats.Stats)
	}

	c.man = drpcmanager.NewWithOptions(tr, opts.Manager)

	return c
}

// Stats returns the collected stats grouped by rpc.
func (c *Conn) Stats() map[string]drpcstats.Stats {
	c.mu.Lock()
	defer c.mu.Unlock()

	stats := make(map[string]drpcstats.Stats, len(c.stats))
	for k, v := range c.stats {
		stats[k] = v.AtomicClone()
	}
	return stats
}

// getStats returns the drpcopts.Stats struct for the given rpc.
func (c *Conn) getStats(rpc string) *drpcstats.Stats {
	c.mu.Lock()
	defer c.mu.Unlock()

	stats := c.stats[rpc]
	if stats == nil {
		stats = new(drpcstats.Stats)
		c.stats[rpc] = stats
	}
	return stats
}

// Transport returns the transport the conn is using.
func (c *Conn) Transport() drpc.Transport { return c.tr }

// Closed returns a channel that is closed once the connection is closed.
func (c *Conn) Closed() <-chan struct{} { return c.man.Closed() }

// Unblocked returns a channel that is closed once the connection is no longer
// blocked by a previously canceled Invoke or NewStream call. It should not
// be called concurrently with Invoke or NewStream.
func (c *Conn) Unblocked() <-chan struct{} { return c.man.Unblocked() }

// Close closes the connection.
func (c *Conn) Close() (err error) { return c.man.Close() }

// Invoke issues the rpc on the transport serializing in, waits for a response, and
// deserializes it into out. Only one Invoke or Stream may be open at a time.
func (c *Conn) Invoke(ctx context.Context, rpc string, enc drpc.Encoding, in, out drpc.Message) error {
	var connID string = fmt.Sprintf("conn-id: %s", c.id)
	var managerErr error

	pprof.Do(ctx, pprof.Labels("drpc-client", connID), func(ctx context.Context) {
		managerErr = c.invoke(ctx, rpc, enc, in, out)
	})

	return managerErr
}

func (c *Conn) invoke(ctx context.Context, rpc string, enc drpc.Encoding, in, out drpc.Message) (invokeErr error) {
	var metadata []byte
	if md, ok := drpcmetadata.Get(ctx); ok {
		metadata, invokeErr = drpcmetadata.Encode(metadata, md)
		if invokeErr != nil {
			return invokeErr
		}
	}

	stream, err := c.man.NewClientStream(ctx, rpc)
	if err != nil {
		return err
	}
	defer func() { invokeErr = errs.Combine(invokeErr, stream.Close()) }()

	// we have to protect c.wbuf here even though the manager only allows one
	// stream at a time because the stream may async close allowing another
	// concurrent call to Invoke to proceed.
	c.mu.Lock()
	defer c.mu.Unlock()

	c.wbuf, invokeErr = drpcenc.MarshalAppend(in, enc, c.wbuf[:0])
	if err != nil {
		return invokeErr
	}

	streamID := fmt.Sprintf("stream-id: %d", stream.ID())
	pprof.Do(ctx, pprof.Labels(streamID, rpc), func(ctx context.Context) {
		if err := c.doInvoke(stream, enc, rpc, c.wbuf, metadata, out); err != nil {
			invokeErr = err
		}
	})

	return invokeErr
}

func (c *Conn) doInvoke(stream *drpcstream.Stream, enc drpc.Encoding, rpc string, data []byte, metadata []byte, out drpc.Message) (err error) {
	if len(metadata) > 0 {
		if err := stream.RawWrite(drpcwire.KindInvokeMetadata, metadata); err != nil {
			return err
		}
	}
	if err := stream.RawWrite(drpcwire.KindInvoke, []byte(rpc)); err != nil {
		return err
	}
	if err := stream.RawWrite(drpcwire.KindMessage, data); err != nil {
		return err
	}
	if err := stream.CloseSend(); err != nil {
		return err
	}
	if err := stream.MsgRecv(out, enc); err != nil {
		return err
	}
	return nil
}

// NewStream begins a streaming rpc on the connection. Only one Invoke or Stream may
// be open at a time.
func (c *Conn) NewStream(ctx context.Context, rpc string, enc drpc.Encoding) (_ drpc.Stream, err error) {
	var metadata []byte
	if md, ok := drpcmetadata.Get(ctx); ok {
		metadata, err = drpcmetadata.Encode(metadata, md)
		if err != nil {
			return nil, err
		}
	}

	stream, err := c.man.NewClientStream(ctx, rpc)
	if err != nil {
		return nil, err
	}

	if err := c.doNewStream(stream, rpc, metadata); err != nil {
		return nil, errs.Combine(err, stream.Close())
	}

	return stream, nil
}

func (c *Conn) doNewStream(stream *drpcstream.Stream, rpc string, metadata []byte) error {
	if len(metadata) > 0 {
		if err := stream.RawWrite(drpcwire.KindInvokeMetadata, metadata); err != nil {
			return err
		}
	}
	if err := stream.RawWrite(drpcwire.KindInvoke, []byte(rpc)); err != nil {
		return err
	}
	return nil
}
