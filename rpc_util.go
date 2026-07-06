package drpc

import (
	"context"
	"io"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ToRPCErr translates a drpc error into a gRPC status error, for callers that
// work in terms of gRPC status codes (for example grpcutil.IsClosedConnection).
//
// It is only a translator. It maps errors that were already classified where
// they happened: the ConnectionError and ClosedError classes (matched even
// through wrapping) and the context and EOF sentinels (matched by identity).
// Anything else becomes codes.Unknown.
//
// The boundary has no way to recover intent that was never attached to the
// error. So every place that builds an error, or gets one from a library it
// calls, must tag it at the source with one of the classes above so it maps to
// a meaningful code. Leave an error unclassified only when it really is
// unexpected. codes.Unknown should mean "we did not anticipate this", not serve
// as a dumping ground for teardown errors. In particular, never bury a sentinel
// like io.EOF or context.Canceled inside an opaque wrapper, because identity
// matching will not see through it. Use a class instead.
func ToRPCErr(err error) error {
	switch err {
	case nil, io.EOF:
		return err
	case context.DeadlineExceeded:
		return status.Error(codes.DeadlineExceeded, err.Error())
	case context.Canceled:
		return status.Error(codes.Canceled, err.Error())
	case io.ErrUnexpectedEOF:
		return status.Error(codes.Internal, err.Error())
	}
	if ConnectionError.Has(err) || ClosedError.Has(err) {
		return status.Error(codes.Unavailable, err.Error())
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	return status.Error(codes.Unknown, err.Error())
}
