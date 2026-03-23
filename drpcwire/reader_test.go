// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcwire

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/zeebo/assert"
)

func TestReadFrame(t *testing.T) {
	f := func(kind Kind, sid, mid uint64, data string, done, control bool) Frame {
		return Frame{
			Data:    []byte(data),
			ID:      ID{Stream: sid, Message: mid},
			Kind:    kind,
			Done:    done,
			Control: control,
		}
	}

	t.Run("SingleFrame", func(t *testing.T) {
		fr := f(KindMessage, 1, 1, "hello", true, false)

		buf := AppendFrame(nil, fr)
		rd := NewReader(bytes.NewReader(buf))

		got, err := rd.ReadFrame()
		assert.NoError(t, err)
		assert.DeepEqual(t, got, fr)

		_, err = rd.ReadFrame()
		assert.That(t, errors.Is(err, io.EOF))
	})

	t.Run("MultipleFrames", func(t *testing.T) {
		// Frames are returned individually even when they share a message
		// ID and have done=false. Reader does no assembly — that's the
		// stream's job.
		frames := []Frame{
			f(KindMessage, 1, 1, "hello", false, false),
			f(KindMessage, 1, 1, " ", false, false),
			f(KindMessage, 1, 1, "world", true, false),
			f(KindClose, 1, 2, "", true, false),
		}

		var buf []byte
		for _, fr := range frames {
			buf = AppendFrame(buf, fr)
		}

		rd := NewReader(bytes.NewReader(buf))
		for _, exp := range frames {
			got, err := rd.ReadFrame()
			assert.NoError(t, err)
			assert.DeepEqual(t, got, exp)
		}

		_, err := rd.ReadFrame()
		assert.That(t, errors.Is(err, io.EOF))
	})

	t.Run("NoMonotonicity", func(t *testing.T) {
		// Reader no longer enforces monotonicity. Frames with decreasing
		// IDs should be returned without error.
		frames := []Frame{
			f(KindMessage, 1, 5, "a", true, false),
			f(KindMessage, 1, 3, "b", true, false),
		}

		var buf []byte
		for _, fr := range frames {
			buf = AppendFrame(buf, fr)
		}

		rd := NewReader(bytes.NewReader(buf))
		for _, exp := range frames {
			got, err := rd.ReadFrame()
			assert.NoError(t, err)
			assert.DeepEqual(t, got, exp)
		}
	})

	t.Run("BufferOverflow_SingleLargeFrame", func(t *testing.T) {
		// 1 more than the maximum frame overhead is the minimum required to overflow.
		const overFrame = maxFrameOverhead + 1
		fr := f(KindMessage, 1, 1, strings.Repeat("X", 4<<20+overFrame), true, false)

		buf := AppendFrame(nil, fr)
		rd := NewReader(bytes.NewReader(buf))

		_, err := rd.ReadFrame()
		assert.Error(t, err)
		assert.That(t, strings.Contains(err.Error(), "data overflow"))
	})

	t.Run("BufferOverflow_CustomLimit", func(t *testing.T) {
		const overFrame = maxFrameOverhead + 1
		fr := f(KindMessage, 1, 1, strings.Repeat("X", 1000+overFrame), true, false)

		buf := AppendFrame(nil, fr)
		rd := NewReaderWithOptions(bytes.NewReader(buf), ReaderOptions{MaximumBufferSize: 1000})

		_, err := rd.ReadFrame()
		assert.Error(t, err)
		assert.That(t, strings.Contains(err.Error(), "data overflow"))
	})

	t.Run("ErrorWithData", func(t *testing.T) {
		// If the underlying reader returns data and an error together,
		// the frame should still be parsed from the data.
		rd := NewReader(readerFunc(func(b []byte) (int, error) {
			out := AppendFrame(b[:0:8], Frame{
				Data: []byte("test"),
				ID:   ID{1, 1},
				Kind: KindMessage,
				Done: true,
			})
			return len(out), io.EOF
		}))

		got, err := rd.ReadFrame()
		assert.NoError(t, err)
		assert.DeepEqual(t, got, Frame{
			Data: []byte("test"),
			ID:   ID{1, 1},
			Kind: KindMessage,
			Done: true,
		})

		_, err = rd.ReadFrame()
		assert.That(t, errors.Is(err, io.EOF))
	})

	t.Run("ErrorNoProgress", func(t *testing.T) {
		rd := NewReader(readerFunc(func(b []byte) (int, error) {
			return 0, nil
		}))

		_, err := rd.ReadFrame()
		assert.That(t, errors.Is(err, io.ErrNoProgress))
	})
}

func TestReadFrame_Randomized(t *testing.T) {
	seed := time.Now().UnixNano()
	t.Log("seed:", seed)
	rng := rand.New(rand.NewSource(seed))

	bid := 0
	get := func(n int) []byte {
		out := make([]byte, n)
		for i := range out {
			out[i] = byte(bid)
			bid++
		}
		return out
	}

	// Build a random sequence of frames with varying sizes.
	var frames []Frame
	var buf []byte

	mid := uint64(1)
	done := false
	for i := 0; i < 1000; i++ {
		data := get(rng.Intn(8192))
		fr := Frame{
			ID:   ID{Stream: 1, Message: mid},
			Data: data,
			Done: done,
		}
		frames = append(frames, fr)
		buf = AppendFrame(buf, fr)

		if done {
			mid++
		}
		done = rng.Intn(10) == 0
	}

	// ReadFrame should return each frame individually.
	bid = 0
	r := NewReader(bytes.NewBuffer(buf))
	for _, exp := range frames {
		got, err := r.ReadFrame()
		assert.NoError(t, err)
		assert.Equal(t, got.ID, exp.ID)
		assert.Equal(t, got.Done, exp.Done)
		assert.Equal(t, got.Data, get(len(exp.Data)))
	}
}

type readerFunc func([]byte) (int, error)

func (fn readerFunc) Read(p []byte) (int, error) { return fn(p) }
