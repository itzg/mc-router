package mcproto

import "fmt"

type Frame struct {
	Length  int
	Payload []byte
}

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
	Data     []byte
}

func (p *Packet) String() string {
	trimmed, cont := trimBytes(p.Data)
	return fmt.Sprintf("Frame:[len=%d, packetId=%d, data=%#X%s]", p.Length, p.PacketID, trimmed, cont)
}

const PacketIdHandshake = 0x00

type Handshake struct {
	ProtocolVersion int
	ServerAddress   string
	ServerPort      uint16
	NextState       int
}

type ByteReader interface {
	ReadByte() (byte, error)
}
