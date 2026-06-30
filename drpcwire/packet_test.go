// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcwire

import (
	"testing"

	"github.com/zeebo/assert"
)

func TestAppendParse(t *testing.T) {
	for i := 0; i < 1000; i++ {
		exp := RandFrame()
		rem, got, ok, err := ParseFrame(AppendFrame(nil, exp))
		assert.NoError(t, err)
		assert.That(t, ok)
		assert.Equal(t, len(rem), 0)
		assert.DeepEqual(t, got, exp)
	}
}

func TestKindWindowUpdateString(t *testing.T) {
	assert.Equal(t, KindWindowUpdate.String(), "WindowUpdate")
}

func TestWindowUpdateFrameRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		stream uint64
		delta  uint64
	}{
		{stream: 7, delta: 128 << 10},    // per-stream
		{stream: 123456, delta: 1 << 40}, // large delta
	} {
		fr := WindowUpdateFrame(tc.stream, tc.delta)
		assert.That(t, fr.Control)
		assert.That(t, fr.Done)
		assert.Equal(t, fr.Kind, KindWindowUpdate)

		rem, got, ok, err := ParseFrame(AppendFrame(nil, fr))
		assert.NoError(t, err)
		assert.That(t, ok)
		assert.Equal(t, len(rem), 0)

		sid, delta, ok := ParseWindowUpdate(got)
		assert.That(t, ok)
		assert.Equal(t, sid, tc.stream)
		assert.Equal(t, delta, tc.delta)
	}
}

func TestParseWindowUpdateRejectsNonconforming(t *testing.T) {
	if _, _, ok := ParseWindowUpdate(WindowUpdateFrame(7, 128)); !ok {
		t.Fatal("conforming window update did not parse")
	}

	notControl := WindowUpdateFrame(7, 128)
	notControl.Control = false
	notDone := WindowUpdateFrame(7, 128)
	notDone.Done = false
	trailing := WindowUpdateFrame(7, 128)
	trailing.Data = append(append([]byte(nil), trailing.Data...), 0xff)
	empty := WindowUpdateFrame(7, 128)
	empty.Data = nil

	for name, fr := range map[string]Frame{
		"wrong kind":     {Kind: KindMessage, Control: true, Done: true, ID: ID{Stream: 7}, Data: AppendVarint(nil, 1)},
		"not control":    notControl,
		"not done":       notDone,
		"stream zero":    WindowUpdateFrame(0, 1),
		"zero delta":     WindowUpdateFrame(7, 0),
		"trailing bytes": trailing,
		"empty payload":  empty,
	} {
		if _, _, ok := ParseWindowUpdate(fr); ok {
			t.Fatalf("expected %q to be rejected", name)
		}
	}
}
