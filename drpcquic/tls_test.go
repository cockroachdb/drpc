// Copyright (C) 2026 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcquic

import (
	"crypto/tls"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnsureALPN_NilGetsDefault(t *testing.T) {
	out := ensureALPN(nil)
	require.NotNil(t, out)
	require.Equal(t, []string{ALPN}, out.NextProtos)
}

func TestEnsureALPN_PreservesExistingAndClones(t *testing.T) {
	in := &tls.Config{NextProtos: []string{"h3"}}
	out := ensureALPN(in)
	require.Equal(t, []string{"h3"}, out.NextProtos) // existing ALPN not overwritten

	out.NextProtos = []string{"mutated"}
	require.Equal(t, []string{"h3"}, in.NextProtos) // input not mutated (cloned)
}
