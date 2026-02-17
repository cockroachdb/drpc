// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcwire

import (
	"github.com/zeebo/errs"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ErrorType represents the version/type of error data serialization format.
type ErrorType uint8

const (
	// ErrorGRPCStatus represents errors serialized as gRPC status protobuf.
	ErrorGRPCStatus ErrorType = 1
)

// MarshalError converts an error to its protobuf representation and returns the
// serialized bytes. It first attempts to extract a gRPC status from the error
// (after unwrapping), and if that fails, it converts the error to a status
// based on standard Go context errors (e.g., context.Canceled,
// context.DeadlineExceeded). The returned data is prefixed with an ErrorType
// byte to indicate the serialization format. If protobuf marshaling fails, it
// returns an empty byte slice.
func MarshalError(err error) []byte {
	if err == nil {
		return []byte{}
	}
	st, ok := status.FromError(errs.Unwrap(err)) //lint:ignore SA1019 errs.Unwrap returns original error for non-wrapped errors, unlike errors.Unwrap which returns nil
	if !ok {
		st = status.FromContextError(err)
	}
	p := st.Proto()
	buf, err := proto.Marshal(p)
	if err != nil {
		return []byte{}
	}

	// Prepend the error version to the marshaled data
	result := make([]byte, 1+len(buf))
	result[0] = byte(ErrorGRPCStatus)
	copy(result[1:], buf)
	return result
}

// UnmarshalError unmarshals the data to a gRPC status.Status and returns it as
// an error. The data should be prefixed with an ErrorType byte followed by
// the serialized error data in the format indicated by the version.
// Returns an error if the data is empty, has an unsupported version, or if
// unmarshaling fails.
func UnmarshalError(data []byte) error {
	if len(data) == 0 {
		return errs.New("drpcwire: empty error data")
	}

	version := ErrorType(data[0])
	payload := data[1:]

	switch version {
	case ErrorGRPCStatus:
		st := &spb.Status{}
		if err := proto.Unmarshal(payload, st); err != nil {
			return errs.New("drpcwire: failed to unmarshal error: %w", err)
		}
		return status.ErrorProto(st)
	default:
		return errs.New("drpcwire: unsupported error version: %d", version)
	}
}
