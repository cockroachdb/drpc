package drpcserver

import (
	"context"
	"log"
	"storj.io/drpc/drpcconn"
	"storj.io/drpc/drpcmigrate"
	"storj.io/drpc/drpcserver/chatpb"
)

func StreamFunction() error {
	ctx := context.Background()
	rawConn, err := drpcmigrate.DialWithHeader(ctx, "tcp", "localhost:9000", drpcmigrate.DRPCHeader)
	if err != nil {
		log.Fatal(err)
	}

	clientConn := drpcconn.New(rawConn)
	defer clientConn.Close()

	// - Create the DRPC client
	client := chatpb.NewDRPCChatServiceClient(clientConn)
	// - Context with timeout
	//ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	//defer cancel()

	// - Open the bidirectional stream
	stream, err := client.ChatStream(ctx)
	if err != nil {
		log.Fatalf("ChatStream error: %v", err)
	}
	// - Send & receive a few messages
	for _, text := range []string{"Hi", "How are you?", "Bye!"} {
		if err := stream.Send(&chatpb.ChatMessage{Sender: "Client", Text: text}); err != nil {
			log.Fatalf("Send error: %v", err)
		}
		resp, err := stream.Recv()
		if err != nil {
			log.Fatalf("Recv error: %v", err)
		}
		log.Printf("Server replied: %s", resp.Text)
	}
	return nil
}
