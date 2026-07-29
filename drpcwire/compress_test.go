package drpcwire

import (
	"bytes"
	"runtime"
	"testing"

	"github.com/zeebo/assert"

	"storj.io/drpc"
)

// allVariants lists every supported compression variant.
var allVariants = []struct {
	name string
	c    drpc.Compression
}{
	{"snappy", drpc.CompressionSnappy},
	{"minlz-fastest", drpc.CompressionMinLZFastest},
}

func TestCompression_RoundtripAllVariants(t *testing.T) {
	for _, v := range allVariants {
		for _, tc := range []struct {
			name string
			data []byte
		}{
			{"empty", nil},
			{"small", []byte("hello world")},
			{"repeated", bytes.Repeat([]byte("abcdefgh"), 1024)},
		} {
			t.Run(v.name+"/"+tc.name, func(t *testing.T) {
				compressed := Compress(v.c, nil, tc.data)
				decompressed, err := Decompress(v.c, nil, compressed)
				assert.NoError(t, err)
				assert.That(t, bytes.Equal(decompressed, tc.data))
			})
		}
	}
}

func TestCompression_RepeatedDataCompressesAllVariants(t *testing.T) {
	data := bytes.Repeat([]byte("cockroachdb"), 1000)
	for _, v := range allVariants {
		t.Run(v.name, func(t *testing.T) {
			compressed := Compress(v.c, nil, data)
			// minlz LevelFastest's pure-Go encoder (every non-amd64 arch)
			// emits a stored block for short-period repetitive inputs like
			// this one, while the amd64 assembly compresses it. If this
			// assertion starts failing on arm64, upstream fixed the gap.
			if v.c == drpc.CompressionMinLZFastest && runtime.GOARCH != "amd64" {
				assert.That(t, len(compressed) >= len(data))
				return
			}
			assert.That(t, len(compressed) < len(data)/2)
		})
	}
}

func TestCompression_CorruptDataAllVariants(t *testing.T) {
	for _, v := range allVariants {
		t.Run(v.name, func(t *testing.T) {
			_, err := Decompress(v.c, nil, []byte("\xff\xfe\xfd not a valid compressed block"))
			assert.Error(t, err)
		})
	}
}

func TestCompression_BufferReuseAllVariants(t *testing.T) {
	data := bytes.Repeat([]byte("reuse me"), 1000)
	for _, v := range allVariants {
		t.Run(v.name, func(t *testing.T) {
			cbuf := Compress(v.c, nil, data)
			cbuf2 := Compress(v.c, cbuf[:0], data)
			assert.Equal(t, &cbuf[:cap(cbuf)][0], &cbuf2[:cap(cbuf2)][0])

			dbuf, err := Decompress(v.c, nil, cbuf2)
			assert.NoError(t, err)
			assert.DeepEqual(t, dbuf, data)
			dbuf2, err := Decompress(v.c, dbuf[:0], cbuf2)
			assert.NoError(t, err)
			assert.Equal(t, &dbuf[:cap(dbuf)][0], &dbuf2[:cap(dbuf2)][0])
			assert.DeepEqual(t, dbuf2, data)
		})
	}
}

func TestMinLZFastest_Roundtrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"small", []byte("hello world")},
		{"at_threshold", bytes.Repeat([]byte("x"), minlzSnappyThreshold)},
		{"above_threshold", bytes.Repeat([]byte("y"), minlzSnappyThreshold+1)},
		{"mid_range_64k", bytes.Repeat([]byte("mid range "), 6400)},
		{"mid_range_1m", bytes.Repeat([]byte("one meg ish "), 90_000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compressed := Compress(drpc.CompressionMinLZFastest, nil, tc.data)
			decompressed, err := Decompress(drpc.CompressionMinLZFastest, nil, compressed)
			assert.NoError(t, err)
			assert.That(t, bytes.Equal(decompressed, tc.data))
		})
	}
}

func TestMinLZFastest_SmallPayloadFallsBackToSnappy(t *testing.T) {
	data := bytes.Repeat([]byte("small"), 500) // 2500 bytes, under 4KiB threshold
	compressed := Compress(drpc.CompressionMinLZFastest, nil, data)
	assert.That(t, len(compressed) < len(data))

	snappyCompressed := Compress(drpc.CompressionSnappy, nil, data)
	assert.That(t, bytes.Equal(compressed, snappyCompressed))

	decompressed, err := Decompress(drpc.CompressionMinLZFastest, nil, compressed)
	assert.NoError(t, err)
	assert.That(t, bytes.Equal(decompressed, data))
}

func TestMinLZFastest_LargeBlockFallsBackToSnappy(t *testing.T) {
	data := bytes.Repeat([]byte("large block fallback test "), 400_000) // ~10MiB
	compressed := Compress(drpc.CompressionMinLZFastest, nil, data)
	assert.That(t, len(compressed) < len(data))

	snappyCompressed := Compress(drpc.CompressionSnappy, nil, data)
	assert.That(t, bytes.Equal(compressed, snappyCompressed))

	decompressed, err := Decompress(drpc.CompressionMinLZFastest, nil, compressed)
	assert.NoError(t, err)
	assert.That(t, bytes.Equal(decompressed, data))
}

func TestMinLZFastest_ThresholdBoundaries(t *testing.T) {
	atThreshold := bytes.Repeat([]byte("a"), minlzSnappyThreshold)
	compAt := Compress(drpc.CompressionMinLZFastest, nil, atThreshold)
	snappyAt := Compress(drpc.CompressionSnappy, nil, atThreshold)
	assert.That(t, bytes.Equal(compAt, snappyAt))

	aboveThreshold := bytes.Repeat([]byte("b"), minlzSnappyThreshold+1)
	compAbove := Compress(drpc.CompressionMinLZFastest, nil, aboveThreshold)
	snappyAbove := Compress(drpc.CompressionSnappy, nil, aboveThreshold)
	assert.That(t, !bytes.Equal(compAbove, snappyAbove))

	for _, tc := range []struct {
		name string
		data []byte
		comp []byte
	}{
		{"at_threshold", atThreshold, compAt},
		{"above_threshold", aboveThreshold, compAbove},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Decompress(drpc.CompressionMinLZFastest, nil, tc.comp)
			assert.NoError(t, err)
			assert.That(t, bytes.Equal(got, tc.data))
		})
	}
}

func TestMinLZFastest_CrossCodecDecode(t *testing.T) {
	data := bytes.Repeat([]byte("cross codec "), 1000)
	snappyCompressed := Compress(drpc.CompressionSnappy, nil, data)

	got, err := Decompress(drpc.CompressionMinLZFastest, nil, snappyCompressed)
	assert.NoError(t, err)
	assert.That(t, bytes.Equal(got, data))
}

func TestCompressionFromName(t *testing.T) {
	c, ok := CompressionFromName("snappy")
	assert.That(t, ok)
	assert.Equal(t, c, drpc.CompressionSnappy)

	c, ok = CompressionFromName("minlz-fastest")
	assert.That(t, ok)
	assert.Equal(t, c, drpc.CompressionMinLZFastest)

	c, ok = CompressionFromName("unknown")
	assert.That(t, !ok)
	assert.Equal(t, c, drpc.CompressionNone)
}

func TestCompressionNone_Name(t *testing.T) {
	assert.Equal(t, CompressionName(drpc.CompressionNone), "")
}

func TestCompressionNone_Passthrough(t *testing.T) {
	data := []byte("hello")
	compressed := Compress(drpc.CompressionNone, nil, data)
	assert.DeepEqual(t, compressed, data)

	decompressed, err := Decompress(drpc.CompressionNone, nil, data)
	assert.NoError(t, err)
	assert.DeepEqual(t, decompressed, data)
}
