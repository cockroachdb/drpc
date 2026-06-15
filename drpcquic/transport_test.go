// Copyright (C) 2026 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcquic

import (
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Passthrough: bytes written via the transport arrive on the peer stream.
func TestStreamTransport_Passthrough(t *testing.T) {
	client, acceptServer, cleanup := newStreamPair(t)
	defer cleanup()

	tr := newStreamTransport(client)
	_, err := tr.Write([]byte("hello"))
	require.NoError(t, err)

	server := acceptServer()
	buf := make([]byte, 5)
	_, err = io.ReadFull(server, buf)
	require.NoError(t, err)
	require.Equal(t, "hello", string(buf))
}

// Regression: Close() MUST unblock a goroutine blocked in Read. drpc's readers
// do deadline-less reads, and Transport.Close() is the only unblock lever; a
// FIN-only close (quic-go Stream.Close half-close) would deadlock here.
func TestStreamTransport_CloseUnblocksBlockedRead(t *testing.T) {
	client, _, cleanup := newStreamPair(t)
	defer cleanup()

	tr := newStreamTransport(client)
	done := make(chan error, 1)
	go func() {
		var b [1]byte
		_, err := tr.Read(b[:]) // no data will ever arrive
		done <- err
	}()

	time.Sleep(50 * time.Millisecond) // let the Read block
	require.NoError(t, tr.Close())

	select {
	case <-done:
		// success: Read returned
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not unblock after Close — deadlock")
	}
}

// Close is idempotent (safe under double-terminate races).
func TestStreamTransport_CloseIdempotent(t *testing.T) {
	client, _, cleanup := newStreamPair(t)
	defer cleanup()

	tr := newStreamTransport(client)
	require.NoError(t, tr.Close())
	require.NoError(t, tr.Close()) // second call must not panic or error
}
