package main

import (
	"log"
	"storj.io/drpc/drpcserver"
)

func main() {
	if err := drpcserver.StreamFunction(); err != nil {
		log.Fatal(err)
	}
}
