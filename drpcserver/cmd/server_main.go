package main

import (
	"log"
	"storj.io/drpc/drpcserver"
)

func main() {
	//if err := drpc.StartServer(); err != nil {
	//	log.Fatal(err)
	//}

	if err := drpcserver.StartChatServer(); err != nil {
		log.Fatal(err)
	}
}
