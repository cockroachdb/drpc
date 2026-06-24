package drpcwire

import (
	"bytes"
	"testing"

	"github.com/zeebo/assert"

	"storj.io/drpc"
)

func TestCompressionSnappy_Roundtrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"small", []byte("hello world")},
		{"repeated", bytes.Repeat([]byte("abcdefgh"), 1024)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compressed := Compress(drpc.CompressionSnappy, nil, tc.data)
			decompressed, err := Decompress(drpc.CompressionSnappy, nil, compressed)
			assert.NoError(t, err)
			assert.DeepEqual(t, decompressed, tc.data)
		})
	}
}

func TestCompressionSnappy_RepeatedDataCompresses(t *testing.T) {
	data := bytes.Repeat([]byte("cockroachdb"), 1000)
	compressed := Compress(drpc.CompressionSnappy, nil, data)
	assert.That(t, len(compressed) < len(data)/2)
}

func TestCompressionSnappy_CorruptData(t *testing.T) {
	_, err := Decompress(drpc.CompressionSnappy, nil, []byte("not valid snappy data"))
	assert.Error(t, err)
}

func TestCompressionSnappy_BufferReuse(t *testing.T) {
	data := bytes.Repeat([]byte("reuse me"), 100)

	cbuf := Compress(drpc.CompressionSnappy, nil, data)
	cbuf2 := Compress(drpc.CompressionSnappy, cbuf[:0], data)
	assert.Equal(t, &cbuf[:cap(cbuf)][0], &cbuf2[:cap(cbuf2)][0])

	got, err := Decompress(drpc.CompressionSnappy, nil, cbuf2)
	assert.NoError(t, err)
	assert.DeepEqual(t, got, data)

	dbuf, err := Decompress(drpc.CompressionSnappy, nil, cbuf2)
	assert.NoError(t, err)
	dbuf2, err := Decompress(drpc.CompressionSnappy, dbuf[:0], cbuf2)
	assert.NoError(t, err)
	assert.Equal(t, &dbuf[:cap(dbuf)][0], &dbuf2[:cap(dbuf2)][0])
	assert.DeepEqual(t, dbuf2, data)
}

func TestCompressionFromName(t *testing.T) {
	c, ok := CompressionFromName("snappy")
	assert.That(t, ok)
	assert.Equal(t, c, drpc.CompressionSnappy)

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
