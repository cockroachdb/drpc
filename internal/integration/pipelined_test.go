// Copyright (C) 2024 Storj Labs, Inc.
// See LICENSE for copying information.

package integration

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/zeebo/assert"

	"storj.io/drpc"
	"storj.io/drpc/drpcwire"
)

//
// muxReader: a multiplexing packet reader that supports out-of-order stream IDs.
// Unlike drpcwire.Reader, it does not enforce monotonically increasing (Stream, Message)
// IDs across streams. Instead, it maintains per-stream reassembly state so that
// frames from different streams can arrive in any order.
//

type muxReader struct {
	r       io.Reader
	curr    []byte
	buf     []byte
	partial map[uint64]*partialPacket
}

type partialPacket struct {
	data []byte
	id   drpcwire.ID
	kind drpcwire.Kind
	ctrl bool
}

func newMuxReader(r io.Reader) *muxReader {
	return &muxReader{
		r:       r,
		curr:    make([]byte, 0, 4096),
		partial: make(map[uint64]*partialPacket),
	}
}

func (r *muxReader) readFrame() (drpcwire.Frame, error) {
	for {
		var fr drpcwire.Frame
		var ok bool
		var err error
		r.curr, fr, ok, err = drpcwire.ParseFrame(r.curr)
		if err != nil {
			return drpcwire.Frame{}, err
		}
		if ok {
			// Got a complete frame. Clear buf so that next time we need
			// more data we start fresh from whatever remains in curr.
			if len(r.buf) > 0 {
				r.buf = r.buf[:0]
			}
			return fr, nil
		}

		// Need more data. Prepend curr to buf if needed.
		if len(r.buf) == 0 {
			r.buf = append(r.buf[:0], r.curr...)
		}
		if cap(r.buf)-len(r.buf) < 4096 {
			nbuf := make([]byte, len(r.buf), 2*cap(r.buf)+4096)
			copy(nbuf, r.buf)
			r.buf = nbuf
		}
		n, err := r.r.Read(r.buf[len(r.buf):cap(r.buf)])
		if err != nil {
			return drpcwire.Frame{}, err
		}
		r.buf = r.buf[:len(r.buf)+n]
		r.curr = r.buf
	}
}

func (r *muxReader) ReadPacket() (drpcwire.Packet, error) {
	for {
		fr, err := r.readFrame()
		if err != nil {
			return drpcwire.Packet{}, err
		}

		sid := fr.ID.Stream
		p := r.partial[sid]

		if p == nil || p.id != fr.ID {
			// New message for this stream.
			p = &partialPacket{
				id:   fr.ID,
				kind: fr.Kind,
				ctrl: fr.Control,
			}
			r.partial[sid] = p
		}

		p.data = append(p.data, fr.Data...)
		p.ctrl = p.ctrl || fr.Control

		if fr.Done {
			pkt := drpcwire.Packet{
				Data:    p.data,
				ID:      p.id,
				Kind:    p.kind,
				Control: p.ctrl,
			}
			delete(r.partial, sid)
			return pkt, nil
		}
	}
}

//
// pipelinedClient: sends requests over a single connection without blocking.
// Uses one background goroutine (readLoop) to dispatch responses to callers.
// Total client goroutines: 2 (main + readLoop), independent of in-flight RPCs.
//

type pendingCall struct {
	out drpc.Message
	enc drpc.Encoding
	cb  func(error) // called on readLoop goroutine
}

type pipelinedClient struct {
	wr *drpcwire.Writer
	rd *muxReader

	mu      sync.Mutex
	nextID  uint64
	pending map[uint64]*pendingCall

	closed chan struct{}
}

func newPipelinedClient(tr drpc.Transport) *pipelinedClient {
	c := &pipelinedClient{
		wr:      drpcwire.NewWriter(tr, 4096),
		rd:      newMuxReader(tr),
		nextID:  1,
		pending: make(map[uint64]*pendingCall),
		closed:  make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// InvokeAsync sends an RPC without blocking. When the response arrives,
// it is unmarshaled into out and cb is called with the error (nil on success).
// cb is called on the reader goroutine.
func (c *pipelinedClient) InvokeAsync(
	rpc string,
	enc drpc.Encoding,
	in, out drpc.Message,
	cb func(error),
) error {
	data, err := enc.Marshal(in)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.nextID
	c.nextID++
	c.pending[id] = &pendingCall{out: out, enc: enc, cb: cb}

	sid := drpcwire.ID{Stream: id, Message: 1}

	if err := c.wr.WritePacket(drpcwire.Packet{
		Data: []byte(rpc), ID: sid, Kind: drpcwire.KindInvoke,
	}); err != nil {
		delete(c.pending, id)
		return err
	}
	sid.Message++

	if err := c.wr.WritePacket(drpcwire.Packet{
		Data: data, ID: sid, Kind: drpcwire.KindMessage,
	}); err != nil {
		delete(c.pending, id)
		return err
	}
	sid.Message++

	if err := c.wr.WritePacket(drpcwire.Packet{
		ID: sid, Kind: drpcwire.KindCloseSend,
	}); err != nil {
		delete(c.pending, id)
		return err
	}

	if err := c.wr.Flush(); err != nil {
		delete(c.pending, id)
		return err
	}

	return nil
}

func (c *pipelinedClient) readLoop() {
	defer close(c.closed)

	for {
		pkt, err := c.rd.ReadPacket()
		if err != nil {
			c.mu.Lock()
			for _, p := range c.pending {
				p.cb(err)
			}
			c.pending = nil
			c.mu.Unlock()
			return
		}

		switch pkt.Kind {
		case drpcwire.KindMessage:
			c.mu.Lock()
			p := c.pending[pkt.ID.Stream]
			delete(c.pending, pkt.ID.Stream)
			c.mu.Unlock()

			if p != nil {
				unmarshalErr := p.enc.Unmarshal(pkt.Data, p.out)
				p.cb(unmarshalErr)
			}

		case drpcwire.KindError:
			c.mu.Lock()
			p := c.pending[pkt.ID.Stream]
			delete(c.pending, pkt.ID.Stream)
			c.mu.Unlock()

			if p != nil {
				p.cb(fmt.Errorf("remote error: %s", pkt.Data))
			}

		case drpcwire.KindCloseSend, drpcwire.KindClose:
			// Stream finished signal — nothing to do.
		}
	}
}

//
// servePipelined: a concurrent server that works at the drpcwire level.
// Reads invocations sequentially (standard drpcwire.Reader — client sends
// in order). Spawns a handler goroutine per RPC. Handlers write responses
// concurrently to the shared drpcwire.Writer (its internal mutex provides
// frame-level atomicity). Responses arrive at the client in arbitrary order.
//

func servePipelined(
	ctx context.Context,
	tr drpc.Transport,
	enc drpc.Encoding,
	handler func(rpc string, reqData []byte) (drpc.Message, error),
) error {
	rd := drpcwire.NewReader(tr)
	wr := drpcwire.NewWriter(tr, 4096)

	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		// Read KindInvoke.
		pkt, err := rd.ReadPacket()
		if err != nil {
			return err
		}
		if pkt.Kind != drpcwire.KindInvoke {
			continue
		}
		rpc := string(pkt.Data)
		streamID := pkt.ID.Stream

		// Read KindMessage.
		pkt, err = rd.ReadPacket()
		if err != nil {
			return err
		}
		reqData := make([]byte, len(pkt.Data))
		copy(reqData, pkt.Data)

		// Read KindCloseSend.
		if _, err := rd.ReadPacket(); err != nil {
			return err
		}

		// Handle concurrently. Server-side goroutines are expected.
		wg.Add(1)
		go func(sid uint64, rpc string, data []byte) {
			defer wg.Done()

			out, hErr := handler(rpc, data)

			id := drpcwire.ID{Stream: sid, Message: 1}
			if hErr != nil {
				_ = wr.WritePacket(drpcwire.Packet{
					Data: []byte(hErr.Error()), ID: id, Kind: drpcwire.KindError,
				})
			} else {
				respData, mErr := enc.Marshal(out)
				if mErr != nil {
					_ = wr.WritePacket(drpcwire.Packet{
						Data: []byte(mErr.Error()), ID: id, Kind: drpcwire.KindError,
					})
				} else {
					_ = wr.WritePacket(drpcwire.Packet{
						Data: respData, ID: id, Kind: drpcwire.KindMessage,
					})
				}
			}
			id.Message++
			_ = wr.WritePacket(drpcwire.Packet{
				ID: id, Kind: drpcwire.KindCloseSend,
			})
			_ = wr.Flush()
		}(streamID, rpc, reqData)
	}
}

//
// Test
//

func TestPipelinedRPC(t *testing.T) {
	const N = 100

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	enc := Encoding

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start pipelined server.
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- servePipelined(ctx, c1, enc,
			func(rpc string, data []byte) (drpc.Message, error) {
				in := new(In)
				if err := enc.Unmarshal(data, in); err != nil {
					return nil, err
				}
				time.Sleep(10 * time.Millisecond)
				return &Out{Out: in.In}, nil
			})
	}()

	// Create pipelined client.
	client := newPipelinedClient(c2)

	type result struct {
		out *Out
		err error
	}
	results := make(chan result, N)

	// Fire 100 RPCs in a single for loop — no goroutine per call.
	start := time.Now()
	for i := int64(0); i < N; i++ {
		out := new(Out)
		err := client.InvokeAsync("/service.Service/Method1", enc,
			&In{In: i}, out,
			func(err error) { results <- result{out: out, err: err} })
		assert.NoError(t, err)
	}

	// Collect all 100 responses (arrive in arbitrary order).
	seen := make(map[int64]bool, N)
	for i := 0; i < N; i++ {
		r := <-results
		assert.NoError(t, r.err)
		assert.That(t, r.out != nil)
		seen[r.out.Out] = true
	}
	assert.Equal(t, N, len(seen))

	// With concurrent server processing: ~10ms total, not 100×10ms = 1s.
	elapsed := time.Since(start)
	t.Logf("elapsed: %v", elapsed)
	assert.That(t, elapsed < 500*time.Millisecond)
}
