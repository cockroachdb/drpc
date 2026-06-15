module storj.io/drpc/examples/drpc

go 1.25.0

require (
	google.golang.org/protobuf v1.30.0
	storj.io/drpc v0.0.17
)

require (
	github.com/golang/protobuf v1.5.3 // indirect
	github.com/zeebo/errs v1.4.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20230525234030-28d5490b6b19 // indirect
	google.golang.org/grpc v1.57.2 // indirect
)

replace storj.io/drpc => ../..
