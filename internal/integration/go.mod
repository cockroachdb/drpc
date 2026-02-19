module storj.io/drpc/internal/integration

go 1.23.0

require (
	github.com/gogo/protobuf v1.3.2
	github.com/zeebo/assert v1.3.1
	github.com/zeebo/errs v1.4.0
	golang.org/x/exp v0.0.0-20240707233637-46b078467d37
	google.golang.org/protobuf v1.33.0
	storj.io/drpc v0.0.0-00010101000000-000000000000
)

require (
	github.com/golang/protobuf v1.5.3 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20230525234030-28d5490b6b19 // indirect
	google.golang.org/grpc v1.57.2 // indirect
)

replace storj.io/drpc => ../..
