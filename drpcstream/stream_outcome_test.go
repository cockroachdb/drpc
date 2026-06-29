// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpcstream

import (
	"context"
	"errors"
	"testing"

	"github.com/zeebo/assert"

	"storj.io/drpc/drpcwire"
)

// TestStreamSucceeded drives a stream to each terminal state through the real
// close/cancel/frame paths and checks that Succeeded classifies graceful
// terminations as success and everything else as failure. Every case first
// asserts the stream actually terminated, so we are classifying a real
// terminal cause rather than an in-flight stream.
func TestStreamSucceeded(t *testing.T) {
	mw := testMuxWriter(t)

	cases := []struct {
		name string
		// drive takes a fresh stream to a terminal state.
		drive func(t *testing.T, st *Stream)
		want  bool
	}{
		{
			name:  "local close is graceful",
			drive: func(t *testing.T, st *Stream) { assert.NoError(t, st.Close()) },
			want:  true,
		},
		{
			name: "remote close is graceful",
			drive: func(t *testing.T, st *Stream) {
				assert.NoError(t, handleFrame(st, drpcwire.KindClose, 1))
			},
			want: true,
		},
		{
			name: "both sides closesend is graceful",
			drive: func(t *testing.T, st *Stream) {
				assert.NoError(t, st.CloseSend())
				assert.NoError(t, handleFrame(st, drpcwire.KindCloseSend, 1))
			},
			want: true,
		},
		{
			name:  "local cancel is a failure",
			drive: func(t *testing.T, st *Stream) { st.Cancel(context.Canceled) },
			want:  false,
		},
		{
			name:  "local send error is a failure",
			drive: func(t *testing.T, st *Stream) { assert.NoError(t, st.SendError(errors.New("boom"))) },
			want:  false,
		},
		{
			name: "remote error is a failure",
			drive: func(t *testing.T, st *Stream) {
				assert.NoError(t, handleFrame(st, drpcwire.KindError, 1))
			},
			want: false,
		},
		{
			name: "remote cancel is a failure",
			drive: func(t *testing.T, st *Stream) {
				assert.NoError(t, handleFrame(st, drpcwire.KindCancel, 1))
			},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := New(context.Background(), 1, mw, NewBufferPool())
			tc.drive(t, st)
			assert.That(t, st.IsTerminated())
			assert.Equal(t, st.Succeeded(), tc.want)
		})
	}
}
