package main

import (
	"context"
	"crypto/tls"
	"fmt"

	pb "drpc-echoservice-quic-stream/echo-proto"

	"storj.io/drpc/drpcquic"
)

func clientTLS() *tls.Config {
	// InsecureSkipVerify accepts the self-signed test cert. The ALPN is forced
	// inside drpcquic, so NextProtos is optional.
	return &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{drpcquic.ALPN},
	}
}

func main() {
	ctx := context.Background()

	// stream-per-QUIC-stream API: Dial takes (ctx, addr, tls) and returns a
	// *QuicConn, which already IS a drpc.Conn — no drpcconn wrapping needed.
	conn, err := drpcquic.Dial(ctx, "localhost:9090", clientTLS())
	if err != nil {
		fmt.Println("unable to dial to server, err:", err)
		return
	}
	defer conn.Close()

	// typed client + call: identical to every other drpc service
	client := pb.NewDRPCEchoServiceClient(conn)

	resp, err := client.Echo(ctx, &pb.EchoRequest{
		Message: "hello\n",
	})
	if err != nil {
		fmt.Println("error getting response, err:", err)
		return
	}

	fmt.Printf("%s", resp.Message)
}
