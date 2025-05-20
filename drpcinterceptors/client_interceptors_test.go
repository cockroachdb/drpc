package drpcinterceptors

import (
	"github.com/zeebo/assert"
	"testing"
)

func TestNoInterceptorsReturnsNil(t *testing.T) {
	// Create empty options with no interceptors
	dialOpts := NewDialOptions([]DialOption{})

	// Chain the interceptors (which should set nil)
	dialOpts.ChainUnaryClientInterceptors()
	dialOpts.ChainStreamClientInterceptors()

	// Verify both interceptors are nil
	assert.Nil(t, dialOpts.UnaryInt)
	assert.Nil(t, dialOpts.StreamInt)
}
