// Copyright (C) 2026 Cockroach Labs.
// See LICENSE for copying information.

package drpc_test

import (
	"testing"

	"github.com/zeebo/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"storj.io/drpc"
)

// A message exceeding a configured size limit is resource-policy behavior, not a
// broken wire protocol, so ToRPCErr maps MessageSizeError to ResourceExhausted
// rather than leaving it as the default codes.Unknown.
func TestToRPCErr_MessageSize(t *testing.T) {
	err := drpc.MessageSizeError.New("message size %d exceeds maximum of %d bytes", 9, 8)
	assert.Equal(t, status.Code(drpc.ToRPCErr(err)), codes.ResourceExhausted)
}
