package main

import (
	"context"
	"crypto/tls"
	"fmt"

	pb "drpc-echoservice-quic-stream/echo-proto"

	"storj.io/drpc/drpcmux"
	"storj.io/drpc/drpcquic"
	"storj.io/drpc/drpcserver"
)

type EchoServer struct{}

func (s *EchoServer) Echo(ctx context.Context, req *pb.EchoRequest) (*pb.EchoResponse, error) {
	fmt.Printf("request: %s", req.Message)

	line := fmt.Sprintf("Echo - %s", req.Message)

	fmt.Printf("response: %s", line)

	return &pb.EchoResponse{
		Message: line,
	}, nil
}

func serverTLS() *tls.Config {
	cert, err := tls.LoadX509KeyPair("cert.pem", "key.pem")
	if err != nil {
		fmt.Println("failed to load cert, err:", err)
		panic(err)
	}

	// The feat/drpc-quic-2 fork forces the ALPN internally (drpcquic.ALPN =
	// "drpc-quic") in both Dial and Listen, so NextProtos is optional here.
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{drpcquic.ALPN},
	}
}

func main() {
	// mux + register: identical to every other drpc service
	m := drpcmux.New()
	pb.DRPCRegisterEchoService(m, &EchoServer{})
	s := drpcserver.New(m)

	// stream-per-QUIC-stream API: Listen takes (addr, tls) and returns a raw
	// *quic.Listener.
	listener, err := drpcquic.Listen("127.0.0.1:9090", serverTLS())
	if err != nil {
		fmt.Println("error listening, err:", err)
		return
	}
	fmt.Println("server started listening on localhost:9090")

	// serving is a METHOD on the server (srv.ServeQuic), not a package-level
	// drpcquic.Serve.
	err = s.ServeQuic(context.Background(), listener)
	if err != nil {
		fmt.Println("server stopped, err:", err)
		return
	}
}
