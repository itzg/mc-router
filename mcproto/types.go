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

/*
Handshaking -> Status
Handshaking -> Login -> ...
*/
const (
	StateHandshaking State = 0
	StateStatus      State = 1
	StateLogin       State = 2
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
	// Data is either a byte slice of raw content or a decoded message
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
	PacketIdLogin                = 0x00 // during StateLogin
	PacketIdLegacyServerListPing = 0xFE
)

type Handshake struct {
	ProtocolVersion int
	ServerAddress   string
	ServerPort      uint16
	NextState       State
}

type LoginStart struct {
	Name       string
	PlayerUuid uuid.UUID
}

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

type StatusResponse struct {
    Version     StatusVersion   `json:"version"`
    Players     StatusPlayers   `json:"players"`
    Description StatusText      `json:"description"`
    Favicon     string          `json:"favicon,omitempty"`
}

type StatusVersion struct {
    Name     string `json:"name"`
    Protocol int    `json:"protocol"`
}

type StatusPlayers struct {
    Max    int           `json:"max"`
    Online int           `json:"online"`
    Sample []PlayerEntry `json:"sample,omitempty"`
}

type PlayerEntry struct {
    Name string `json:"name"`
    ID   string `json:"id"`
}

type StatusText struct {
    Text string `json:"text"`
}