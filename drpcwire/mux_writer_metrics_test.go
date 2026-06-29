// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcwire

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zeebo/assert"
)

// TestMuxWriter_OnBlockedLen verifies that the blocked-producer hook reports the
// parked-producer count: it stays at zero while writes proceed, rises to one
// when a producer parks on backpressure, and returns to zero once the producer
// is unblocked.
func TestMuxWriter_OnBlockedLen(t *testing.T) {
	bw := newBlockingWriter()

	var mu sync.Mutex
	var lastBlocked, maxBlocked int
	observe := func() (last, max int) {
		mu.Lock()
		defer mu.Unlock()
		return lastBlocked, maxBlocked
	}

	mw := NewMuxWriterWithOptions(bw, func(error) {}, WriterOptions{
		MaximumBufferSize: 1, // tiny high-water mark forces backpressure
		OnBlockedLen: func(n int) {
			mu.Lock()
			defer mu.Unlock()
			lastBlocked = n
			if n > maxBlocked {
				maxBlocked = n
			}
		},
	})

	// Stall run() inside Write and fill the pending buffer past the limit. No
	// producer has parked yet, so the hook has not fired.
	blockUntilFull(t, mw, bw)
	if _, max := observe(); max != 0 {
		t.Fatalf("blocked count rose to %d before any producer parked", max)
	}

	// This frame parks on backpressure; the hook reports one parked producer.
	done := make(chan error, 1)
	go func() { done <- mw.WriteFrame(RandFrame(), nil) }()

	waitBlocked := func(want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			last, _ := observe()
			if last == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("blocked count = %d, want %d", last, want)
			}
			time.Sleep(time.Millisecond)
		}
	}

	waitBlocked(1)
	if _, max := observe(); max != 1 {
		t.Fatalf("max blocked = %d, want 1", max)
	}

	// Releasing the stalled Write drains the buffer and wakes the parked
	// producer, which appends and returns; the count drops back to zero.
	close(bw.unblock)
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("parked WriteFrame stayed blocked after drain")
	}
	waitBlocked(0)

	mw.Stop(errors.New("stopped"))
	<-mw.Done()
}
