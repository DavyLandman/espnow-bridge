package bridge

import (
	"bytes"
	"errors"
	"github.com/snksoft/crc"
	"go.bug.st/serial.v1"
	"io"
	"log"
	"time"
)

type Message struct {
	Mac  [6]byte
	Data []byte
}

type peer struct {
	mac         [6]byte
	wifiChannel uint8
}

type Bridge struct {
	connection io.ReadWriteCloser
	peers      []peer
	Inbox      <-chan Message
	Outbox     chan<- Message
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
		log.Printf("Opened Serial port\n")
		return b.setupBridge()
	}
	return errors.New("Already initialized")
}

func (b Bridge) Close() error {
	if b.connection != nil {
		close(b.Outbox)
		b.connection.Close()
		b.connection = nil
	}
	return nil
}

func (b Bridge) AddPeer(mac [6]byte, wifiChannel uint8) error {
	if len(b.peers) >= 20 {
		return errors.New("ESP8266 can handle only 19 peers, remove peers first")
	}
	newPeer := peer{wifiChannel: wifiChannel}
	copy(newPeer.mac[:], mac[:])
	b.peers = append(b.peers)
	return nil
}

func (b Bridge) RemovePeer(mac [6]byte) {
	// find peer
	peerIndex := -1
	for i := 0; i < len(b.peers); i++ {
		if bytes.Equal(b.peers[i].mac[:], mac[:]) {
			peerIndex = i
			break
		}
	}
	if peerIndex != -1 {
		// shift the last peer entry to replace the deleted peer
		// and truncate the slice
		b.peers[peerIndex] = b.peers[len(b.peers)-1]
		b.peers = b.peers[:len(b.peers)-1]
	}
}

func (b Bridge) setupBridge() error {
	if b.connection == nil {
		panic("setup called without a connection")
	}
	bytesRead := make(chan byte, 1024)
	inbox := make(chan Message, 64)
	outbox := make(chan Message, 64)
	reset := make(chan bool)
	sendPeers := make(chan bool)
	go readBytes(b.connection, bytesRead)
	go reassembleMessages(bytesRead, reset, sendPeers, inbox)
	go writeBytes(&b, outbox, reset, sendPeers)

	b.Inbox = inbox
	b.Outbox = outbox
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
		b, more := <-input
		if !more {
			return nil, false
		}
		result[i] = b
	}
	return result, true
}

func reassembleMessages(input <-chan byte, reset chan<- bool, sendPeers chan<- bool, output chan<- Message) {
	activationHeader := [...]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77}
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
				case b, running := <-input:
					if !running {
						return
					}
					if b == activationHeader[detected] {
						detected++
					} else {
						detected = 0
					}
				case <-time.After(10 * time.Second):
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
		case header[0] == 0x55 && header[1] == 0x44:
			// new message, read next bytes for the structure
			mac, running := getBytes(input, 6)
			crc, running := getBytes(input, 2)
			size, running := <-input
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
			msg := Message{
				Data: data,
			}
			copy(msg.Mac[:], mac)
			output <- msg
		case header[0] == 0x44 && header[1] == 0x33:
			// request to get all peers (restart of the node for example)
			log.Println("Request to get all peers received")
			sendPeers <- true
		default:
			log.Printf("Resetting stream due to unexpected message header %v\n", header)
			active = false
			continue
		}
	}

}

func assureWritten(target io.ReadWriteCloser, data []byte) {
	index := 0
	for index < len(data) {
		written, failed := target.Write(data[index:])
		if failed != nil {
			log.Fatal(failed)
		}
		index += written
	}
}

func writeBytes(b *Bridge, box <-chan Message, reset <-chan bool, sendPeers <-chan bool) {
	sendMessage := []byte{0x22, 0x11}
	resetMessage := []byte{0x42, 0x42, 0x42, 0x42}
	addPeer := []byte{0x33, 0x22}
	crcFunction := crc.NewHashWithTable(crc.NewTable(crc.XMODEM))

	for {
		select {
		case msg := <-box:
			if len(msg.Data) > 250 {
				log.Fatal("Should not send more than 250 bytes, esp-now can not handle that")
			}
			assureWritten(b.connection, sendMessage)
			assureWritten(b.connection, msg.Mac[:])
			crc := crcFunction.CalculateCRC(msg.Data)
			assureWritten(b.connection, []byte{uint8(crc & 0xFF), uint8((crc >> 8) & 0xFF)})
			assureWritten(b.connection, []byte{uint8(len(msg.Data))})
			assureWritten(b.connection, msg.Data)
		case <-reset:
			assureWritten(b.connection, resetMessage)
		case <-sendPeers:
			for _, p := range b.peers {
				assureWritten(b.connection, addPeer)
				assureWritten(b.connection, p.mac[:])
				assureWritten(b.connection, []byte{p.wifiChannel})
			}
		}
	}
}
