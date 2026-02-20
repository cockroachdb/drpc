// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package integration

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/zeebo/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"storj.io/drpc/drpctest"
)

func TestError(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cli, close := createConnection(t, standardImpl)
	defer close()

	for i := int64(2); i < 20; i++ {
		out, err := cli.Method1(ctx, in(i))
		assert.Nil(t, out)
		assert.Error(t, err)
		st, ok := status.FromError(err)
		assert.That(t, ok)
		assert.Equal(t, st.Code(), codes.Code(i))
	}
}

func TestError_Context(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cli, close := createConnection(t, impl{
		Method1Fn: func(ctx context.Context, in *In) (*Out, error) {
			return nil, [...]error{
				context.Canceled,
				context.DeadlineExceeded,
			}[in.In%2]
		},
	})
	defer close()

	for i := int64(2); i < 20; i++ {
		out, err := cli.Method1(ctx, in(i))
		assert.Nil(t, out)
		assert.Error(t, err)
		assert.That(t, strings.Contains(err.Error(), "context"))
	}
}

func TestError_UnitaryNilResponse(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cli, close := createConnection(t, impl{
		Method1Fn: func(ctx context.Context, in *In) (*Out, error) {
			return nil, nil
		},
	})
	defer close()

	out, err := cli.Method1(ctx, in(1))
	assert.Equal(t, err, io.EOF)
	assert.Nil(t, out)
}

func TestError_Message(t *testing.T) {
	ctx := drpctest.NewTracker(t)
	defer ctx.Close()

	cli, close := createConnection(t, impl{
		Method1Fn: func(ctx context.Context, in *In) (*Out, error) {
			return nil, errors.New("some unique error message")
		},
	})
	defer close()

	out, err := cli.Method1(ctx, in(1))
	assert.Nil(t, out)
	assert.Error(t, err)
	st, ok := status.FromError(err)
	assert.That(t, ok)
	assert.Equal(t, st.Message(), "some unique error message")
}
