package mcproto

import (
	"fmt"

	"github.com/google/uuid"
)

type Frame struct {
	Length  int
	Payload []byte
}

type State int

const (
	StateHandshaking = iota
)

var trimLimit = 64

func trimBytes(data []byte) ([]byte, string) {
	if len(data) < trimLimit {
		return data, ""
	} else {
		return data[:trimLimit], "..."
	}
}

func (f *Frame) String() string {
	trimmed, cont := trimBytes(f.Payload)
	return fmt.Sprintf("Frame:[len=%d, payload=%#X%s]", f.Length, trimmed, cont)
}

type Packet struct {
	Length   int
	PacketID int
	// Data is either a byte slice of raw content or a parsed message
	Data interface{}
}

func (p *Packet) String() string {
	if dataBytes, ok := p.Data.([]byte); ok {
		trimmed, cont := trimBytes(dataBytes)
		return fmt.Sprintf("Frame:[len=%d, packetId=%d, data=%#X%s]", p.Length, p.PacketID, trimmed, cont)
	} else {
		return fmt.Sprintf("Frame:[len=%d, packetId=%d, data=%+v]", p.Length, p.PacketID, p.Data)
	}
}

const (
	PacketIdHandshake            = 0x00
	PacketIdLogin                = 0x00
	PacketIdLegacyServerListPing = 0xFE
)

type Handshake struct {
	ProtocolVersion int
	ServerAddress   string
	ServerPort      uint16
	NextState       int
}

type Login struct {
	Name string
	PlayerUUID uuid.UUID
}

const (
	StateStatus State = 1
	StateLogin State  = 2
)

type LegacyServerListPing struct {
	ProtocolVersion int
	ServerAddress   string
	ServerPort      uint16
}

type ByteReader interface {
	ReadByte() (byte, error)
}

const (
	PacketLengthFieldBytes = 1
)
