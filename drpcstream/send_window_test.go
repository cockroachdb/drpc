// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/zeebo/assert"
	"github.com/zeebo/errs"
)

// blockShort is how long we wait to conclude that an acquire is (correctly)
// blocked before we release it.
const blockShort = 20 * time.Millisecond

// newTestWindow returns a window seeded with initial credit plus a terminate
// func that closes its done channel; a terminated acquire returns termErr.
func newTestWindow(initial int64, termErr error) (*sendWindow, func()) {
	done := make(chan struct{})
	w := newSendWindow(initial, done, func() error { return termErr })
	return w, func() { close(done) }
}

func TestSendWindowAcquireImmediate(t *testing.T) {
	w, _ := newTestWindow(1000, nil)
	assert.Equal(t, w.available(), int64(1000))

	assert.NoError(t, w.acquire(400))
	assert.Equal(t, w.available(), int64(600))

	assert.NoError(t, w.acquire(600))
	assert.Equal(t, w.available(), int64(0))
}

func TestSendWindowGrantsAccumulate(t *testing.T) {
	w, _ := newTestWindow(0, nil)
	w.grant(100)
	w.grant(50)
	assert.Equal(t, w.available(), int64(150))

	assert.NoError(t, w.acquire(150))
	assert.Equal(t, w.available(), int64(0))
}

func TestSendWindowAcquireBlocksUntilGrant(t *testing.T) {
	w, _ := newTestWindow(100, nil)
	done := make(chan error, 1)
	go func() { done <- w.acquire(300) }()

	// Not enough credit yet: acquire debits into a deficit and blocks.
	select {
	case <-done:
		t.Fatal("acquire returned before sufficient credit")
	case <-time.After(blockShort):
	}
	assert.Equal(t, w.available(), int64(-200))

	w.grant(250) // -200 + 250 = 50 >= 0

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("acquire did not return after grant")
	}
	assert.Equal(t, w.available(), int64(50))
}

// A grant that only partially repays the deficit does not wake the acquire; the
// grant that clears it does.
func TestSendWindowPartialGrantsThenClear(t *testing.T) {
	w, _ := newTestWindow(0, nil)
	done := make(chan error, 1)
	go func() { done <- w.acquire(300) }()

	select {
	case <-done:
		t.Fatal("acquire returned before any credit")
	case <-time.After(blockShort):
	}

	w.grant(250) // -300 + 250 = -50, still negative -> stays blocked
	select {
	case <-done:
		t.Fatal("acquire returned while still in deficit")
	case <-time.After(blockShort):
	}

	w.grant(100) // -50 + 100 = 50 >= 0 -> wakes
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("acquire did not return after the deficit cleared")
	}
	assert.Equal(t, w.available(), int64(50))
}

func TestSendWindowGrantSaturates(t *testing.T) {
	// Adding to a near-max balance saturates at MaxInt64 instead of wrapping.
	w, _ := newTestWindow(math.MaxInt64-10, nil)
	w.grant(100)
	assert.Equal(t, w.available(), int64(math.MaxInt64))

	// A single delta larger than MaxInt64 (the wire delta is uint64) saturates
	// rather than becoming a negative balance.
	w2, _ := newTestWindow(0, nil)
	w2.grant(math.MaxUint64)
	assert.Equal(t, w2.available(), int64(math.MaxInt64))
	assert.That(t, w2.available() > 0) // never wrapped negative

	// A grant that raises a negative balance is applied exactly (no saturation).
	w3, _ := newTestWindow(0, nil)
	w3.avail.Store(-300)
	w3.grant(500)
	assert.Equal(t, w3.available(), int64(200))

	// A very large grant against a negative balance repays the debt before
	// clamping: the true sum -300 + MaxInt64 fits, so it must not saturate to
	// MaxInt64 (which would erase the 300 of pre-enforcement debt).
	w4, _ := newTestWindow(0, nil)
	w4.avail.Store(-300)
	w4.grant(math.MaxInt64)
	assert.Equal(t, w4.available(), int64(math.MaxInt64-300))

	// Only when the true sum truly overflows does it clamp.
	w5, _ := newTestWindow(0, nil)
	w5.avail.Store(-300)
	w5.grant(math.MaxUint64)
	assert.Equal(t, w5.available(), int64(math.MaxInt64))
}

func TestSendWindowDoneWakesAcquire(t *testing.T) {
	closeErr := errs.New("terminated")
	w, terminate := newTestWindow(0, closeErr)
	done := make(chan error, 1)
	go func() { done <- w.acquire(100) }()

	select {
	case <-done:
		t.Fatal("acquire returned before termination")
	case <-time.After(blockShort):
	}

	terminate()

	select {
	case err := <-done:
		assert.That(t, errors.Is(err, closeErr))
	case <-time.After(time.Second):
		t.Fatal("acquire did not wake on termination")
	}
}

func TestSendWindowAcquireNonPositive(t *testing.T) {
	w, _ := newTestWindow(100, nil)
	assert.NoError(t, w.acquire(0))
	assert.NoError(t, w.acquire(-5))
	// Non-positive acquire consumes nothing; a negative one must not add credit.
	assert.Equal(t, w.available(), int64(100))
}
