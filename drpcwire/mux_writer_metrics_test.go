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

// TestMuxWriter_OnQueueLen verifies that the queue-length hook reports the
// pending-buffer byte depth: it goes positive as frames are appended and
// returns to zero once a flush swaps the buffer out.
func TestMuxWriter_OnQueueLen(t *testing.T) {
	bw := newBlockingWriter()

	var mu sync.Mutex
	var last, max int
	observe := func() (lastN, maxN int) {
		mu.Lock()
		defer mu.Unlock()
		return last, max
	}

	mw := NewMuxWriterWithOptions(bw, func(error) {}, WriterOptions{
		OnQueueLen: func(n int) {
			mu.Lock()
			defer mu.Unlock()
			last = n
			if n > max {
				max = n
			}
		},
	})

	waitFor := func(cond func(last, max int) bool, msg string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			l, m := observe()
			if cond(l, m) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s (last=%d max=%d)", msg, l, m)
			}
			time.Sleep(time.Millisecond)
		}
	}

	// First frame: run() picks it up and stalls in Write, leaving buf empty.
	assert.NoError(t, mw.WriteFrame(RandFrame(), nil))
	select {
	case <-bw.wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("run() did not enter Write")
	}
	// Second frame accumulates in the pending buffer.
	assert.NoError(t, mw.WriteFrame(RandFrame(), nil))

	// The appends pushed the pending length positive.
	waitFor(func(_, m int) bool { return m > 0 }, "pending length never went positive")

	// Releasing the stalled Write drains the buffer; the flush-swap reports zero
	// pending bytes.
	close(bw.unblock)
	waitFor(func(l, _ int) bool { return l == 0 }, "pending length never returned to zero")

	mw.Stop(errors.New("stopped"))
	<-mw.Done()
}

// TestMuxWriter_OnQueueLenZeroOnStop verifies that stopping the writer while
// bytes are still pending drives the queue-length hook back to zero, so a
// torn-down connection's gauge does not stick at its last non-zero reading.
func TestMuxWriter_OnQueueLenZeroOnStop(t *testing.T) {
	bw := newBlockingWriter()

	var mu sync.Mutex
	var last, max int
	observe := func() (lastN, maxN int) {
		mu.Lock()
		defer mu.Unlock()
		return last, max
	}

	mw := NewMuxWriterWithOptions(bw, func(error) {}, WriterOptions{
		OnQueueLen: func(n int) {
			mu.Lock()
			defer mu.Unlock()
			last = n
			if n > max {
				max = n
			}
		},
	})

	waitFor := func(cond func(last, max int) bool, msg string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			l, m := observe()
			if cond(l, m) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s (last=%d max=%d)", msg, l, m)
			}
			time.Sleep(time.Millisecond)
		}
	}

	// First frame: run() picks it up and stalls in Write, leaving buf empty.
	assert.NoError(t, mw.WriteFrame(RandFrame(), nil))
	select {
	case <-bw.wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("run() did not enter Write")
	}
	// Second frame stays pending in the buffer behind the stalled Write.
	assert.NoError(t, mw.WriteFrame(RandFrame(), nil))
	waitFor(func(l, _ int) bool { return l > 0 }, "pending length never went positive")

	// Stop while those bytes are still pending. Releasing the stalled Write lets
	// run() observe the close and exit; on the way out it must report an empty
	// queue rather than leaving the gauge at its last positive reading.
	mw.Stop(errors.New("stopped"))
	close(bw.unblock)
	<-mw.Done()

	waitFor(func(l, _ int) bool { return l == 0 }, "queue length not zeroed on stop")
}
