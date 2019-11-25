package main

import (
	"log"
	"os"
	"time"

	espnow "github.com/DavyLandman/espnow-bridge"
)

func printMessages(msgQue <-chan espnow.Message) {
	for msg := range msgQue {
		log.Printf("Gotten message: %v from %v\n", len(msg.Data), msg.Mac)
	}
}

func main() {
	br := new(espnow.Bridge)
	defer br.Close()
	if err := br.Connect(os.Args[1]); err != nil {
		log.Fatal(err)
	}
	br.WaitForConnected(10 * time.Second)
	br.AddPeer([6]byte{0x2e, 0xf4, 0x32, 0x12, 0xd5, 0x73}, 1)

	printMessages(br.Inbox)
}
