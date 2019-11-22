package bridge;

import (
		"log"
		"errors"
		"io"
		"time"
		"go.bug.st/serial.v1"
		"github.com/snksoft/crc"
)

type Message struct {
	Mac [6]byte
	Data []byte
}

type client struct {
	mac [6]byte
	wifiChannel uint8
}

type Bridge struct {
	connection io.ReadWriteCloser
	clients []client
	Inbox <-chan Message
	Outbox chan<- Message
}

func (b Bridge) Connect(portName string) error {
	if b.connection == nil {
		mode := &serial.Mode{
			BaudRate: 460800,
		}
		con, err := serial.Open(portName, mode)
		if err != nil {
			return err
		}
		b.connection = con
		log.Printf("Opened Serial port\n");
		return b.setupBridge()
	}
	return errors.New("Already initialized")
}

func (b Bridge) Close() error {
	if b.connection != nil {
		b.connection.Close()
		b.connection = nil
	}
	return nil
}

func (b Bridge) setupBridge() error {
	if b.connection == nil {
		panic("setup called without a connection")
	}
	bytesRead := make(chan byte, 1024) 
	inbox := make(chan Message, 64)
	outbox := make(chan Message, 64)
	b.Inbox = inbox
	b.Outbox = outbox 
	reset := make(chan bool)
	sendSignatures := make(chan bool)
	go readBytes(b.connection, bytesRead)
	go reassemblMessages(bytesRead, reset, sendSignatures, inbox)
	go b.handleResets(reset, sendSignatures)
	go handleNewMessages(b.connection, outbox)
	return nil
}



func readBytes(source io.ReadWriteCloser, output chan<- byte) {
	defer close(output)
	buf := make([]byte, 256)
	for {
		n, err := source.Read(buf)
		if err != nil {
			log.Fatal(err)
			return
		}
		for i := 0; i < n; i++ {
			output <- buf[i]
		}
	}
}

func getBytes(input <-chan byte, len int) ([]byte, bool) {
	result := make([]byte, len)
	for i := 0; i < len; i++ {
		b, more := <- input
		if !more {
			return nil, false
		}
		result[i] = b
	}
	return result, true
}

func reassemblMessages(input <-chan byte, reset chan<- bool, sendSignatures chan<- bool, output chan<- Message) {
	activationHeader := [...]byte{0x11, 0x22, 0x33, 0x44, 0x55,0x66,0x77}
	crcFunction := crc.NewHashWithTable(crc.NewTable(crc.XMODEM))

	var active = false
	defer close(output)
	for {
		if !active {
			log.Println("Waiting for connection to bridge")
			reset <- true 
			var detected = 0
			for detected < len(activationHeader) {
				select {
				case b, running := <- input:
					if !running {
						return
					}
					if b == activationHeader[detected] {
						detected++
					} else {
						detected = 0
					}
				case <- time.After(10 * time.Second):
					log.Println("Sending reset again")
					reset <- true 
				}
			}
			log.Println("Bridge connected")
			active = true
		}
		header, running := getBytes(input, 2)
		if !running {
			return
		}
		switch {
		case header[0] == 0x55 && header[1] == 0x44 :
			// new message, read next bytes for the structure
			mac, running := getBytes(input, 6)
			crc, running := getBytes(input, 2)
			size, running := <- input 
			if !running {
				return
			}
			data, running := getBytes(input, int(size))
			if !running {
				return
			}
			log.Printf("Got data: %v", data)
			dataCRC := crcFunction.CalculateCRC(data)
			if (uint16(crc[0]) | (uint16(crc[1]) << 8)) != uint16(dataCRC) {
				log.Println("Resetting stream due to crc failure")
				active = false
				continue
			}
			log.Println("Posting new message")
			msg := Message {
				Data: data,
			}
			copy(msg.Mac[:], mac)
			output <- msg
		case header[0] == 0x44 && header[1] == 0x33:
			// request to get all peers (restart of the node for example)
			log.Println("Request to get all peers received")
			sendSignatures <- true
		default:
			log.Printf("Resetting stream due to unexpected message header %v\n", header)
			active = false
			continue
		}
	}

}

func assureWritten(target io.ReadWriteCloser, data []byte) error {
	index := 0 
	for index < len(data) {
		written, failed := target.Write(data[index:])
		if failed != nil {
			return failed
		}
		index += written
	}
	return nil
}

func (b Bridge) handleResets(reset <-chan bool, sendPeers <-chan bool) {
	resetMessage := []byte{0x42, 0x42, 0x42, 0x42} 
	addPeer := []byte{0x33, 0x22}
	for {
		select {
		case <- reset:
			err := assureWritten(b.connection, resetMessage)
			if err != nil{
				log.Fatal(err)
			}
		case <- sendPeers:
			for _, p := range b.clients {
				msg := addPeer
				msg = append(msg, p.mac[:]...)
				msg = append(msg, p.wifiChannel)
				err := assureWritten(b.connection, msg)
				if err != nil{
					log.Fatal(err)
				}
			}

		}
	}
}

func handleNewMessages(target io.ReadWriteCloser, box <-chan Message) {
	sendMessage := []byte{0x22, 0x11}
	for msg := range box {
		bytes := sendMessage
		bytes = append(bytes, msg.Mac[:]...)
		bytes = append(bytes, uint8(len(msg.Data)))
		bytes = append(bytes, msg.Data...)
		assureWritten(target, bytes)
	}
}