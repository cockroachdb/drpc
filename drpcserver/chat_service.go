package drpcserver

import (
	"context"
	"log"
	"net"
	"storj.io/drpc"
	"storj.io/drpc/drpcmigrate"
	"storj.io/drpc/drpcmux"
	"storj.io/drpc/drpcserver/chatpb"
	"time"
)

type chatServer struct {
	chatpb.DRPCChatServiceUnimplementedServer
}

func (s *chatServer) ChatStream(stream chatpb.DRPCChatService_ChatStreamStream) error {
	for {
		msg, err := stream.Recv()
		if err != nil {
			log.Printf("stream closed or errored: %v", err)
			return err
		}

		log.Printf("Received from %s: %s", msg.Sender, msg.Text)

		// Echo back the message with a prefix
		response := &chatpb.ChatMessage{
			Sender: "Server",
			Text:   "Echo: " + msg.Text,
		}

		if err := stream.Send(response); err != nil {
			return err
		}
	}
}

// loggingStreamWrapper wraps a drpc.Stream to intercept and log messages.
type loggingStreamWrapper struct {
	drpc.Stream // Embed the original stream to inherit its methods
	rpcName     string
}

// MsgSend logs the message being sent and then calls the underlying stream's MsgSend.
func (lsw *loggingStreamWrapper) MsgSend(m drpc.Message, enc drpc.Encoding) error {
	log.Printf("Server Interceptor (%s): Sending message: %+v", lsw.rpcName, m)
	return lsw.Stream.MsgSend(m, enc)
}

// MsgRecv calls the underlying stream's MsgRecv and then logs the received message.
func (lsw *loggingStreamWrapper) MsgRecv(m drpc.Message, enc drpc.Encoding) error {
	err := lsw.Stream.MsgRecv(m, enc)
	if err == nil {
		log.Printf("Server Interceptor (%s): Received message: %+v", lsw.rpcName, m)
	} else {
		// Avoid logging the message if Recv failed, as 'm' might not be valid.
		// The error itself will be logged by the RPC completion log or handled by the service.
	}
	return err
}

// messageLoggingInterceptor is a server interceptor that logs RPC calls and individual messages.
func messageLoggingInterceptor(ctx context.Context, rpc string, stream drpc.Stream, handler drpc.Handler) error {
	start := time.Now()
	log.Printf("Server Interceptor: RPC Method %s called", rpc)

	// Wrap the stream to log messages
	loggedStream := &loggingStreamWrapper{Stream: stream, rpcName: rpc}

	// Call the next handler in the chain with the wrapped stream.
	err := handler.HandleRPC(loggedStream, rpc)

	duration := time.Since(start)
	if err != nil {
		log.Printf("Server Interceptor: RPC Method %s completed with error: %v (Duration: %s)", rpc, err, duration)
	} else {
		log.Printf("Server Interceptor: RPC Method %s completed successfully (Duration: %s)", rpc, duration)
	}
	return err
}

func StartChatServer() error {
	listener, err := net.Listen("tcp", ":9000")
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}
	defer listener.Close()

	// create a listen mux that evalutes enough bytes to recognize the DRPC header
	lisMux := drpcmigrate.NewListenMux(listener, len(drpcmigrate.DRPCHeader))
	// Start the mux in a background goroutine
	go func() {
		if err := lisMux.Run(context.Background()); err != nil {
			log.Fatalf("ListenMux run failed: %v", err)
		}
	}()
	// grab the listen mux route for the DRPC Header and default listener
	drpcLis := lisMux.Route(drpcmigrate.DRPCHeader)

	mux := drpcmux.New()
	if err := chatpb.DRPCRegisterChatService(mux, &chatServer{}); err != nil {
		log.Fatalf("Failed to register: %v", err)
	}
	server := NewWithOptions(mux, Options{}, WithChainServerInterceptor(messageLoggingInterceptor))

	log.Println("DRPC server listening on :9000")
	//for {
	//	conn, err := listener.Accept()
	//	if err != nil {
	//		log.Printf("Accept error: %v", err)
	//		continue
	//	}
	//	go func(conn net.Conn) {
	//		ctx := context.Background()
	//		if err := server.ServeOne(ctx, conn); err != nil {
	//			log.Printf("Serve error: %v", err)
	//		}
	//	}(conn)
	//}
	//
	// run the server
	// N.B.: if you want TLS, you need to wrap the net.Listener with
	// TLS before passing to Serve here.
	ctx := context.Background()
	return server.Serve(ctx, drpcLis)
	return nil
}
