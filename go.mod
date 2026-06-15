module storj.io/drpc

go 1.25.0

require (
	github.com/quic-go/quic-go v0.0.0-00010101000000-000000000000
	github.com/stretchr/testify v1.11.1
	github.com/zeebo/assert v1.3.1
	github.com/zeebo/errs v1.4.0
	google.golang.org/genproto/googleapis/api v0.0.0-20230525234035-dd9d682886f9
	google.golang.org/genproto/googleapis/rpc v0.0.0-20230525234030-28d5490b6b19
	google.golang.org/grpc v1.57.2
	google.golang.org/protobuf v1.30.0
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/golang/protobuf v1.5.3 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	golang.org/x/crypto v0.51.0 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	google.golang.org/genproto v0.0.0-20230526161137-0005af68ea54 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/quic-go/quic-go => ../quic-go
