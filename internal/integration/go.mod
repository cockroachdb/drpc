module storj.io/drpc/internal/integration

go 1.25.0

require (
	github.com/cockroachdb/errors v1.12.0
	github.com/gogo/protobuf v1.3.2
	github.com/zeebo/assert v1.3.1
	github.com/zeebo/errs v1.4.0
	golang.org/x/exp v0.0.0-20260218203240-3dfff04db8fa
	google.golang.org/grpc v1.57.2
	google.golang.org/protobuf v1.33.0
	storj.io/drpc v0.0.0-00010101000000-000000000000
)

require (
	github.com/cockroachdb/logtags v0.0.0-20230118201751-21c54148d20b // indirect
	github.com/cockroachdb/redact v1.1.5 // indirect
	github.com/getsentry/sentry-go v0.27.0 // indirect
	github.com/golang/protobuf v1.5.3 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/rogpeppe/go-internal v1.9.0 // indirect
	golang.org/x/sys v0.33.0 // indirect
	golang.org/x/text v0.23.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20230525234030-28d5490b6b19 // indirect
)

replace storj.io/drpc => ../..
