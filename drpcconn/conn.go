// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcconn

import (
	"context"

	"github.com/zeebo/errs"
	grpcmetadata "google.golang.org/grpc/metadata"
	"storj.io/drpc"
	"storj.io/drpc/drpcenc"
	"storj.io/drpc/drpcmanager"
	"storj.io/drpc/drpcmetadata"
	"storj.io/drpc/drpcmetrics"
	"storj.io/drpc/drpcstream"
	"storj.io/drpc/drpcwire"
)

// Options controls configuration settings for a conn.
type Options struct {
	// Manager controls the options we pass to the manager of this conn.
	Manager drpcmanager.Options

	// ShouldRecord controls whether connection metrics are recorded. A nil
	// function disables collection.
	ShouldRecord func() bool

	// Metrics holds metrics the connection will populate. Its zero value
	// records nothing.
	Metrics drpcmetrics.ConnectionMetrics
}

// Conn is a drpc client connection.
type Conn struct {
	tr  drpc.Transport
	man *drpcmanager.Manager
}

var _ drpc.Conn = (*Conn)(nil)

// New returns a conn that uses the transport for reads and writes.
func New(tr drpc.Transport) *Conn { return NewWithOptions(tr, Options{}) }

// NewWithOptions returns a conn that uses the transport for reads and writes.
// The Options control details of how the conn operates.
func NewWithOptions(tr drpc.Transport, opts Options) *Conn {
	c := &Conn{
		tr: tr,
	}

	shouldWrap := opts.ShouldRecord != nil
	if opts.ShouldRecord == nil {
		opts.ShouldRecord = func() bool { return false }
	}
	if shouldWrap {
		mt := drpcmetrics.ToMeteredTransport(tr, opts.Metrics.BytesSent,
			opts.Metrics.BytesRecv, opts.ShouldRecord)
		c.tr = mt
	}

	opts.Manager.Metrics = opts.Metrics
	opts.Manager.ShouldRecord = opts.ShouldRecord
	c.man = drpcmanager.NewWithOptions(c.tr, drpcmanager.Client, opts.Manager)

	return c
}

// Transport returns the transport the conn is using.
func (c *Conn) Transport() drpc.Transport { return c.tr }

// Closed returns a channel that is closed once the connection is closed.
func (c *Conn) Closed() <-chan struct{} { return c.man.Closed() }

// Unblocked returns a channel that is closed once the connection is no longer
// blocked. With multiplexing, multiple streams run concurrently and this
// channel is always closed immediately.
func (c *Conn) Unblocked() <-chan struct{} { return c.man.Unblocked() }

// Close closes the connection.
func (c *Conn) Close() (err error) { return c.man.Close() }

// Invoke issues the rpc on the transport serializing in, waits for a response, and
// deserializes it into out.
func (c *Conn) Invoke(ctx context.Context, rpc string, enc drpc.Encoding, in, out drpc.Message) (err error) {
	defer func() { err = drpc.ToRPCErr(err) }()

	var metadata []byte
	metadata, err = c.encodeMetadata(ctx)
	if err != nil {
		return err
	}

	stream, err := c.man.NewClientStream(ctx, rpc)
	if err != nil {
		return err
	}
	defer func() { err = errs.Combine(err, stream.Close()) }()

	// TODO: use buffer pool to reduce allocations
	data, err := drpcenc.MarshalAppend(in, enc, nil)
	if err != nil {
		return err
	}

	if err := c.doInvoke(stream, enc, rpc, data, metadata, out); err != nil {
		return err
	}
	return nil
}

func (c *Conn) doInvoke(stream *drpcstream.Stream, enc drpc.Encoding, rpc string, data []byte, metadata []byte, out drpc.Message) (err error) {
	defer func() { err = stream.CheckCancelError(err) }()
	if err := stream.WriteInvoke(rpc, metadata); err != nil {
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

// NewStream begins a streaming rpc on the connection.
func (c *Conn) NewStream(ctx context.Context, rpc string, enc drpc.Encoding) (_ drpc.Stream, err error) {
	defer func() { err = drpc.ToRPCErr(err) }()

	var metadata []byte
	metadata, err = c.encodeMetadata(ctx)
	if err != nil {
		return nil, err
	}

	stream, err := c.man.NewClientStream(ctx, rpc)
	if err != nil {
		return nil, err
	}

	if err := stream.WriteInvoke(rpc, metadata); err != nil {
		return nil, errs.Combine(err, stream.Close())
	}

	return stream, nil
}

// encodeMetadata retrieves and encodes metadata from the provided
// (outgoing/client) context.
func (c *Conn) encodeMetadata(ctx context.Context) (metadata []byte, err error) {
	md, _ := drpcmetadata.GetFromOutgoingContext(ctx)
	// Look for grpc metadata in the context and merge them with the drpc metadata,
	// prioritizing drpc values when keys overlap. This is a short-term fix
	// that will enable us to send and receive metadata when DRPC is enabled,
	// without any changes in the calling code (which can continue to use
	// the grpc metadata package to send and receive metadata).
	if grpcMd, ok := grpcmetadata.FromOutgoingContext(ctx); ok {
		if md == nil {
			md = make(map[string]string)
		}
		for k, v := range grpcMd {
			// If a key is present in both, we keep the drpc metadata.
			if _, ok := md[k]; !ok && len(v) > 0 {
				// When a key has multiple values, only the first value
				// is used.
				md[k] = v[0]
			}
		}
	}
	if len(md) > 0 {
		metadata, err = drpcmetadata.Encode(metadata, md)
		if err != nil {
			return nil, err
		}
	}
	return metadata, nil
}
