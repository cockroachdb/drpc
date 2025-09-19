// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcwire

import (
	"context"
	"errors"
	"testing"

	"github.com/zeebo/assert"
	"github.com/zeebo/errs"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestMarshalUnmarshalError_GRPCStatus(t *testing.T) {
	// Test with a gRPC status error
	originalErr := status.Error(codes.NotFound, "resource not found")

	data := MarshalError(originalErr)
	unmarshaled := UnmarshalError(data)

	// Extract status from unmarshaled error
	st, ok := status.FromError(unmarshaled)
	assert.That(t, ok)
	assert.Equal(t, st.Code(), codes.NotFound)
	assert.Equal(t, st.Message(), "resource not found")
}

func TestMarshalUnmarshalError_WrappedGRPCStatus(t *testing.T) {
	// Test with a wrapped gRPC status error
	originalErr := status.Error(codes.InvalidArgument, "bad request")
	wrappedErr := errs.Wrap(originalErr)

	data := MarshalError(wrappedErr)
	unmarshaled := UnmarshalError(data)

	// Extract status from unmarshaled error
	st, ok := status.FromError(unmarshaled)
	assert.That(t, ok)
	assert.Equal(t, st.Code(), codes.InvalidArgument)
	assert.Equal(t, st.Message(), "bad request")
}

func TestMarshalUnmarshalError_ContextErrors(t *testing.T) {
	testCases := []struct {
		name         string
		err          error
		expectedCode codes.Code
	}{
		{
			name:         "context.Canceled",
			err:          context.Canceled,
			expectedCode: codes.Canceled,
		},
		{
			name:         "context.DeadlineExceeded",
			err:          context.DeadlineExceeded,
			expectedCode: codes.DeadlineExceeded,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			data := MarshalError(tc.err)
			unmarshaled := UnmarshalError(data)

			st, ok := status.FromError(unmarshaled)
			assert.That(t, ok)
			assert.Equal(t, st.Code(), tc.expectedCode)
		})
	}
}

func TestMarshalUnmarshalError_RegularError(t *testing.T) {
	// Test with a regular error (should be converted to Unknown status)
	originalErr := errors.New("something went wrong")

	data := MarshalError(originalErr)
	unmarshaled := UnmarshalError(data)

	// Regular errors get converted to Unknown status
	st, ok := status.FromError(unmarshaled)
	assert.That(t, ok)
	assert.Equal(t, st.Code(), codes.Unknown)
	assert.Equal(t, st.Message(), "something went wrong")
}

func TestUnmarshalError_InvalidData(t *testing.T) {
	// Test with invalid protobuf data (valid version but invalid protobuf)
	invalidData := []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF} // version 1 + invalid protobuf
	err := UnmarshalError(invalidData)
	assert.That(t, err != nil)
	// Check that it contains the expected error prefix
	errMsg := err.Error()
	expected := "drpcwire: failed to unmarshal error"
	assert.That(t, len(errMsg) >= len(expected))
	assert.Equal(t, errMsg[:len(expected)], expected)
}

func TestMarshalError_NilError(t *testing.T) {
	// Test marshaling a nil error (should not panic)
	data := MarshalError(nil)
	assert.Equal(t, len(data), 0)

	err := UnmarshalError(data)
	assert.That(t, err != nil)
	assert.Equal(t, err.Error(), "drpcwire: empty error data")
}

func TestMarshalUnmarshalError_RoundTrip(t *testing.T) {
	// Test various error types for round-trip consistency
	testCases := []error{
		status.Error(codes.Internal, "internal server error"),
		status.Error(codes.PermissionDenied, "access denied"),
		status.Error(codes.Unimplemented, "method not implemented"),
		context.Canceled,
		context.DeadlineExceeded,
		errors.New("generic error"),
		errs.New("zeebo error"),
	}

	for i, originalErr := range testCases {
		t.Run(string(rune('A'+i)), func(t *testing.T) {
			// Marshal and unmarshal
			data := MarshalError(originalErr)
			assert.That(t, len(data) > 0)

			unmarshaled := UnmarshalError(data)
			assert.That(t, unmarshaled != nil)

			// Both should be valid gRPC status errors after processing
			_, ok := status.FromError(unmarshaled)
			assert.That(t, ok)
		})
	}
}

func TestMarshalUnmarshalError_ComplexWrapping(t *testing.T) {
	// Test deeply wrapped errors
	baseErr := status.Error(codes.Internal, "base error")
	wrapped1 := errs.Wrap(baseErr)
	wrapped2 := errs.Wrap(wrapped1)
	wrapped3 := errs.Wrap(wrapped2)

	data := MarshalError(wrapped3)
	unmarshaled := UnmarshalError(data)

	st, ok := status.FromError(unmarshaled)
	assert.That(t, ok)
	assert.Equal(t, st.Code(), codes.Internal)
	assert.Equal(t, st.Message(), "base error")
}

func TestErrorType_CorrectVersionInMarshaledData(t *testing.T) {
	// Test that marshaled data starts with the correct version byte
	originalErr := status.Error(codes.NotFound, "resource not found")
	data := MarshalError(originalErr)

	assert.That(t, len(data) > 0)
	assert.Equal(t, ErrorType(data[0]), ErrorGRPCStatus)
}

func TestUnmarshalError_UnsupportedVersion(t *testing.T) {
	// Test with unsupported version
	unsupportedData := []byte{0xFF, 0x01, 0x02, 0x03} // version 255 + some data
	err := UnmarshalError(unsupportedData)
	assert.That(t, err != nil)
	assert.Equal(t, err.Error(), "drpcwire: unsupported error version: 255")
}

func TestUnmarshalError_TooShortData(t *testing.T) {
	// Test with data too short to contain version
	shortData := []byte{}
	err := UnmarshalError(shortData)
	assert.That(t, err != nil)
	assert.Equal(t, err.Error(), "drpcwire: empty error data")
}
