module drpc-echoservice-quic-stream

go 1.26.3

replace storj.io/drpc => ../..

require (
	google.golang.org/protobuf v1.36.11
	storj.io/drpc v0.0.0-00010101000000-000000000000
)

require (
	github.com/golang/protobuf v1.5.3 // indirect
	github.com/quic-go/quic-go v0.59.1 // indirect
	github.com/zeebo/errs v1.4.0 // indirect
	golang.org/x/crypto v0.51.0 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20230525234030-28d5490b6b19 // indirect
	google.golang.org/grpc v1.57.2 // indirect
)
