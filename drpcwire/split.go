// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcwire

// SplitData is used to split a buffer if it is larger than n bytes.
// If n is zero, a reasonable default is used. If n is less than zero
// then it does not split.
func SplitData(buf []byte, n int) (prefix, suffix []byte) {
	switch {
	case n == 0:
		n = 64 * 1024
	case n < 0:
		n = 0
	}

	if len(buf) > n && n > 0 {
		return buf[:n], buf[n:]
	}
	return buf, nil
}
