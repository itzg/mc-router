package mcproto

import (
	"bytes"
	"errors"
	"io"
	"strings"
)

type Frame struct {
	Length  int
	Payload []byte
}

type Packet struct {
	Length   int
	PacketID int
	Data     []byte
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

func ReadVarInt(reader io.Reader) (int, error) {
	b := make([]byte, 1)
	var numRead uint = 0
	result := 0
	for numRead <= 5 {
		n, err := reader.Read(b)
		if err != nil {
			return 0, err
		}
		if n == 0 {
			continue
		}
		value := b[0] & 0x7F
		result |= int(value) << (7 * numRead)

		numRead++

		if b[0]&0x80 == 0 {
			return result, nil
		}
	}

	return 0, errors.New("VarInt is too big")
}

func ReadString(reader io.Reader) (string, error) {
	length, err := ReadVarInt(reader)
	if err != nil {
		return "", err
	}

	b := make([]byte, 1)
	var strBuilder strings.Builder
	for i := 0; i < length; i++ {
		n, err := reader.Read(b)
		if err != nil {
			return "", err
		}
		if n == 0 {
			continue
		}
		strBuilder.WriteByte(b[0])
	}

	return strBuilder.String(), nil
}

func ReadUnsignedShort(reader io.Reader) (uint16, error) {
	upper := make([]byte, 1)
	_, err := reader.Read(upper)
	if err != nil {
		return 0, err
	}
	lower := make([]byte, 1)
	_, err = reader.Read(lower)
	if err != nil {
		return 0, err
	}

	return (uint16(upper[0]) << 8) | uint16(lower[0]), nil
}

func ReadFrame(reader io.Reader) (*Frame, error) {
	var err error
	frame := &Frame{}

	frame.Length, err = ReadVarInt(reader)
	if err != nil {
		return nil, err
	}

	frame.Payload = make([]byte, frame.Length)
	total := 0
	for total < frame.Length {
		readIntoThis := frame.Payload[total:]
		n, err := reader.Read(readIntoThis)
		if err != nil {
			if err != io.EOF {
				return nil, err
			}
		}
		total += n
	}

	return frame, nil
}

func ReadPacket(reader io.Reader) (*Packet, error) {

	frame, err := ReadFrame(reader)
	if err != nil {
		return nil, err
	}

	packet := &Packet{Length: frame.Length}

	remainder := bytes.NewBuffer(frame.Payload)

	packet.PacketID, err = ReadVarInt(remainder)
	if err != nil {
		return nil, err
	}

	packet.Data = remainder.Bytes()

	return packet, nil
}

func ReadHandshake(data []byte) (*Handshake, error) {

	handshake := &Handshake{}
	buffer := bytes.NewBuffer(data)
	var err error

	handshake.ProtocolVersion, err = ReadVarInt(buffer)
	if err != nil {
		return nil, err
	}

	handshake.ServerAddress, err = ReadString(buffer)
	if err != nil {
		return nil, err
	}

	handshake.ServerPort, err = ReadUnsignedShort(buffer)
	if err != nil {
		return nil, err
	}

	nextState, err := ReadVarInt(buffer)
	if err != nil {
		return nil, err
	}
	handshake.NextState = nextState
	return handshake, nil
}
