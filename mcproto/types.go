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

type ProtocolVersion int

// Source: https://minecraft.wiki/w/Minecraft_Wiki:Projects/wiki.vg_merge/Protocol_History
const (
	// ProtocolVersion1_18_2 is the protocol version for Minecraft 1.18.2
	// Docs: https://minecraft.wiki/w/Java_Edition_protocol/Packets?oldid=2772791
	ProtocolVersion1_18_2 ProtocolVersion = 758
	// ProtocolVersion1_19 is the protocol version for Minecraft 1.19
	// Docs: https://minecraft.wiki/w/Java_Edition_protocol/Packets?oldid=2772904
	ProtocolVersion1_19 ProtocolVersion = 759
	// ProtocolVersion1_19_2 is the protocol version for Minecraft 1.19.2
	// Docs: https://minecraft.wiki/w/Java_Edition_protocol/Packets?oldid=2772944
	ProtocolVersion1_19_2 ProtocolVersion = 760
	// ProtocolVersion1_19_2 is the protocol version for Minecraft 1.19.3
	ProtocolVersion1_19_3 ProtocolVersion = 761
	// ProtocolVersion1_20_2 is the protocol version for Minecraft 1.20.2
	ProtocolVersion1_20_2 ProtocolVersion = 764
	// ProtocolVersion1_21_5 is the protocol version for Minecraft 1.21.5
	ProtocolVersion1_21_5 ProtocolVersion = 770
)

const (
	PacketIdHandshake            = 0x00
	PacketIdLogin                = 0x00 // during StateLogin
	PacketIdLegacyServerListPing = 0xFE
	PacketIdStatusRequest        = 0x00
	PacketIdStatusPing           = 0x01
)

type Handshake struct {
	ProtocolVersion ProtocolVersion
	ServerAddress   string
	ServerPort      uint16
	NextState       State
}

type LoginStart struct {
	Name       string
	PlayerUuid uuid.UUID
}

func NewLoginStart() *LoginStart {
	return &LoginStart{
		// Note: This is indistinguishable between no UUID provided, and a provided UUID of all 0s
		PlayerUuid: uuid.Nil,
	}
}

type LegacyServerListPing struct {
	ProtocolVersion int
	ServerAddress   string
	ServerPort      uint16
}

// PingPayload represents the status ping payload (packet 0x01)
type PingPayload struct {
	Value int64
}

type ByteReader interface {
	ReadByte() (byte, error)
}

const (
	PacketLengthFieldBytes = 1
)
