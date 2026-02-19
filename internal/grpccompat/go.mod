module storj.io/drpc/internal/grpccompat

go 1.23.0

require (
	github.com/zeebo/assert v1.3.1
	github.com/zeebo/errs v1.4.0
	google.golang.org/grpc v1.64.0
	google.golang.org/protobuf v1.34.1
	storj.io/drpc v0.0.0-00010101000000-000000000000
)

require (
	github.com/golang/protobuf v1.5.4 // indirect
	golang.org/x/net v0.22.0 // indirect
	golang.org/x/sys v0.33.0 // indirect
	golang.org/x/text v0.14.0 // indirect
	google.golang.org/genproto v0.0.0-20210126160654-44e461bb6506 // indirect
)

replace storj.io/drpc => ../..
