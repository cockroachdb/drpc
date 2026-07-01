// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import (
	"context"
	"errors"
	"io"
	"math"
	"testing"
	"time"

	"github.com/zeebo/assert"
	"github.com/zeebo/errs"
)

// blockShort is how long we wait to conclude that an acquire is (correctly)
// blocked before we release it.
const blockShort = 20 * time.Millisecond

func TestSendWindowAcquireImmediate(t *testing.T) {
	w := newSendWindow(1000)
	assert.Equal(t, w.available(), int64(1000))

	assert.NoError(t, w.acquire(context.Background(), 400))
	assert.Equal(t, w.available(), int64(600))

	assert.NoError(t, w.acquire(context.Background(), 600))
	assert.Equal(t, w.available(), int64(0))
}

func TestSendWindowGrantsAccumulate(t *testing.T) {
	w := newSendWindow(0)
	w.grant(100)
	w.grant(50)
	assert.Equal(t, w.available(), int64(150))

	assert.NoError(t, w.acquire(context.Background(), 150))
	assert.Equal(t, w.available(), int64(0))
}

func TestSendWindowAcquireBlocksUntilGrant(t *testing.T) {
	w := newSendWindow(100)
	done := make(chan error, 1)
	go func() { done <- w.acquire(context.Background(), 300) }()

	// Not enough credit yet: acquire must block.
	select {
	case <-done:
		t.Fatal("acquire returned before sufficient credit")
	case <-time.After(blockShort):
	}

	w.grant(250) // 100 + 250 = 350 >= 300

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("acquire did not return after grant")
	}
	assert.Equal(t, w.available(), int64(50))
}

func TestSendWindowAcquireContextCancel(t *testing.T) {
	w := newSendWindow(0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.acquire(ctx, 100) }()

	select {
	case <-done:
		t.Fatal("acquire returned before cancellation")
	case <-time.After(blockShort):
	}

	cancel()

	select {
	case err := <-done:
		assert.That(t, errors.Is(err, context.Canceled))
	case <-time.After(time.Second):
		t.Fatal("acquire did not wake on context cancellation")
	}
	// Credit was not consumed by a failed acquire.
	assert.Equal(t, w.available(), int64(0))
}

func TestSendWindowGrantSaturates(t *testing.T) {
	// Adding to a near-max balance saturates at MaxInt64 instead of wrapping.
	w := newSendWindow(math.MaxInt64 - 10)
	w.grant(100)
	assert.Equal(t, w.available(), int64(math.MaxInt64))

	// A single delta larger than MaxInt64 (the wire delta is uint64) saturates
	// rather than becoming a negative balance.
	w2 := newSendWindow(0)
	w2.grant(math.MaxUint64)
	assert.Equal(t, w2.available(), int64(math.MaxInt64))
	assert.That(t, w2.available() > 0) // never wrapped negative

	// A grant that raises a negative balance is applied exactly (no saturation).
	w3 := newSendWindow(0)
	w3.avail = -300
	w3.grant(500)
	assert.Equal(t, w3.available(), int64(200))

	// A very large grant against a negative balance repays the debt before
	// clamping: the true sum -300 + MaxInt64 fits, so it must not saturate to
	// MaxInt64 (which would erase the 300 of pre-enforcement debt).
	w4 := newSendWindow(0)
	w4.avail = -300
	w4.grant(math.MaxInt64)
	assert.Equal(t, w4.available(), int64(math.MaxInt64-300))

	// Only when the true sum truly overflows does it clamp.
	w5 := newSendWindow(0)
	w5.avail = -300
	w5.grant(math.MaxUint64)
	assert.Equal(t, w5.available(), int64(math.MaxInt64))
}

func TestSendWindowAcquireCanceledCtxWithCredit(t *testing.T) {
	w := newSendWindow(1000)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled, with credit available

	err := w.acquire(ctx, 100)
	assert.That(t, errors.Is(err, context.Canceled))
	// A canceled context must not consume credit even though it was available.
	assert.Equal(t, w.available(), int64(1000))
}

func TestSendWindowCloseWakesAcquire(t *testing.T) {
	w := newSendWindow(0)
	closeErr := errs.New("terminated")
	done := make(chan error, 1)
	go func() { done <- w.acquire(context.Background(), 100) }()

	select {
	case <-done:
		t.Fatal("acquire returned before close")
	case <-time.After(blockShort):
	}

	w.close(closeErr)

	select {
	case err := <-done:
		assert.That(t, errors.Is(err, closeErr))
	case <-time.After(time.Second):
		t.Fatal("acquire did not wake on close")
	}
}

func TestSendWindowAcquireAfterClose(t *testing.T) {
	w := newSendWindow(1000)
	closeErr := errs.New("closed")
	w.close(closeErr)

	// Even though credit is available, a closed window returns the close error.
	assert.That(t, errors.Is(w.acquire(context.Background(), 1), closeErr))
}

func TestSendWindowCloseNilError(t *testing.T) {
	w := newSendWindow(1000)
	w.close(nil) // closing with nil must not let a later acquire report success
	assert.That(t, errors.Is(w.acquire(context.Background(), 1), io.EOF))
}

func TestSendWindowAcquireNonPositive(t *testing.T) {
	w := newSendWindow(100)
	assert.NoError(t, w.acquire(context.Background(), 0))
	assert.NoError(t, w.acquire(context.Background(), -5))
	// Non-positive acquire consumes nothing; a negative one must not add credit.
	assert.Equal(t, w.available(), int64(100))
}

func TestSendWindowAcquireZeroObservesTerminalState(t *testing.T) {
	// A zero-length acquire (empty frame) must still observe a canceled context...
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.That(t, errors.Is(newSendWindow(100).acquire(ctx, 0), context.Canceled))

	// ...and a closed window, rather than reporting success.
	w := newSendWindow(100)
	closeErr := errs.New("terminated")
	w.close(closeErr)
	assert.That(t, errors.Is(w.acquire(context.Background(), 0), closeErr))
}
