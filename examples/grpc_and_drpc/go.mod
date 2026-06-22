module storj.io/drpc/examples/grpc_and_drpc

go 1.25.0

require (
	golang.org/x/sync v0.20.0
	google.golang.org/grpc v1.57.2
	google.golang.org/protobuf v1.30.0
	storj.io/drpc v0.0.17
)

require (
	github.com/golang/protobuf v1.5.3 // indirect
	github.com/zeebo/errs v1.4.0 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20230525234030-28d5490b6b19 // indirect
)

replace storj.io/drpc => ../..
