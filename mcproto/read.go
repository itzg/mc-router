// Package mcproto provides functions to read types and decode frames declared
// at https://minecraft.wiki/w/Java_Edition_protocol
package mcproto

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// MaxFrameLength is declared at https://minecraft.wiki/w/Java_Edition_protocol#Packet_format
// to be 2^21 - 1
const MaxFrameLength = 2097151

// ReadPacket reads a packet from the given reader based on the provided connection state.
// Returns a pointer to the Packet and an error if reading fails.
// Handles legacy server list ping packet when in the handshaking state.
// The provided addr is used for logging purposes.
func ReadPacket(reader *bufio.Reader, addr net.Addr, state State) (*Packet, error) {
	logrus.
		WithField("client", addr).
		Trace("Reading packet")

	if state == StateHandshaking {
		data, err := reader.Peek(1)
		if err != nil {
			return nil, err
		}

		if data[0] == PacketIdLegacyServerListPing {
			return ReadLegacyServerListPing(reader, addr)
		}
	}

	frame, err := ReadFrame(reader, addr)
	if err != nil {
		return nil, err
	}

	// Packet length is frame length (bytes for packetID and data) plus bytes used to store the frame length data
	packet := &Packet{Length: frame.Length + PacketLengthFieldBytes}

	remainder := bytes.NewBuffer(frame.Payload)

	packet.PacketID, err = ReadVarInt(remainder)
	if err != nil {
		return nil, err
	}

	packet.Data = remainder.Bytes()

	logrus.
		WithField("client", addr).
		WithField("packet", packet).
		Debug("Read packet")
	return packet, nil
}

func ReadLegacyServerListPing(reader *bufio.Reader, addr net.Addr) (*Packet, error) {
	logrus.
		WithField("client", addr).
		Debug("Reading legacy server list ping")

	packetId, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	if packetId != PacketIdLegacyServerListPing {
		return nil, errors.Errorf("expected legacy server listing ping packet ID, got %x", packetId)
	}

	payload, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	if payload != 0x01 {
		return nil, errors.Errorf("expected payload=1 from legacy server listing ping, got %x", payload)
	}

	packetIdForPluginMsg, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	if packetIdForPluginMsg != 0xFA {
		return nil, errors.Errorf("expected packetIdForPluginMsg=0xFA from legacy server listing ping, got %x", packetIdForPluginMsg)
	}

	messageNameShortLen, err := ReadUnsignedShort(reader)
	if err != nil {
		return nil, err
	}
	if messageNameShortLen != 11 {
		return nil, errors.Errorf("expected messageNameShortLen=11 from legacy server listing ping, got %d", messageNameShortLen)
	}

	messageName, err := ReadUTF16BEString(reader, messageNameShortLen)
	if err != nil {
		return nil, err
	}
	if messageName != "MC|PingHost" {
		return nil, errors.Errorf("expected messageName=MC|PingHost, got %s", messageName)
	}

	remainingLen, err := ReadUnsignedShort(reader)
	if err != nil {
		return nil, err
	}
	remainingReader := io.LimitReader(reader, int64(remainingLen))

	protocolVersion, err := ReadByte(remainingReader)
	if err != nil {
		return nil, err
	}

	hostnameLen, err := ReadUnsignedShort(remainingReader)
	if err != nil {
		return nil, err
	}
	hostname, err := ReadUTF16BEString(remainingReader, hostnameLen)
	if err != nil {
		return nil, err
	}

	port, err := ReadUnsignedInt(remainingReader)
	if err != nil {
		return nil, err
	}

	return &Packet{
		PacketID: PacketIdLegacyServerListPing,
		Length:   0,
		Data: &LegacyServerListPing{
			ProtocolVersion: int(protocolVersion),
			ServerAddress:   hostname,
			ServerPort:      uint16(port),
		},
	}, nil
}

func ReadUTF16BEString(reader io.Reader, symbolLen uint16) (string, error) {
	bsUtf16be := make([]byte, symbolLen*2)

	_, err := io.ReadFull(reader, bsUtf16be)
	if err != nil {
		return "", err
	}

	result, _, err := transform.Bytes(unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM).NewDecoder(), bsUtf16be)
	if err != nil {
		return "", err
	}

	return string(result), nil
}

func ReadFrame(reader io.Reader, addr net.Addr) (*Frame, error) {
	logrus.
		WithField("client", addr).
		Trace("Reading frame")

	var err error
	frame := &Frame{}

	frame.Length, err = ReadVarInt(reader)
	if err != nil {
		return nil, err
	}

	if frame.Length > MaxFrameLength {
		return nil, errors.Errorf("frame length %d too large", frame.Length)
	}

	logrus.
		WithField("client", addr).
		WithField("length", frame.Length).
		Debug("Read frame length")

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
		logrus.
			WithField("client", addr).
			WithField("total", total).
			WithField("length", frame.Length).
			Debug("Reading frame content")

		if n == 0 {
			logrus.
				WithField("client", addr).
				WithField("frame", frame).
				Debug("No progress on frame reading")

			time.Sleep(100 * time.Millisecond)
		}
	}

	logrus.
		WithField("client", addr).
		WithField("frame", frame).
		Debug("Read frame")
	return frame, nil
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

func ReadBoolean(reader io.Reader) (bool, error) {
	byteVal, err := ReadByte(reader)
	if err != nil {
		return false, err
	}
	switch byteVal {
	case 0x00:
		return false, nil
	case 0x01:
		return true, nil
	default:
		return false, errors.Errorf("expected 0x00 or 0x01 for boolean, got 0x%02X", byteVal)
	}
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

func ReadByte(reader io.Reader) (byte, error) {
	buf := make([]byte, 1)
	_, err := reader.Read(buf)
	if err != nil {
		return 0, err
	} else {
		return buf[0], nil
	}
}

func ReadByteArray(reader io.Reader, length int) ([]byte, error) {
	if length < 0 {
		return nil, errors.New("length cannot be negative")
	}

	data := make([]byte, length)
	_, err := io.ReadFull(reader, data)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func ReadUnsignedShort(reader io.Reader) (uint16, error) {
	var value uint16
	err := binary.Read(reader, binary.BigEndian, &value)
	if err != nil {
		return 0, err
	}
	return value, nil
}

func ReadUnsignedInt(reader io.Reader) (uint32, error) {
	var value uint32
	err := binary.Read(reader, binary.BigEndian, &value)
	if err != nil {
		return 0, err
	}
	return value, nil
}

func ReadLong(reader io.Reader) (int64, error) {
	var value int64
	err := binary.Read(reader, binary.BigEndian, &value)
	if err != nil {
		return 0, err
	}
	return value, nil
}

func ReadUuid(reader io.Reader) (uuid.UUID, error) {
	uuidBytes := make([]byte, 16)
	_, err := io.ReadFull(reader, uuidBytes)
	if err != nil {
		return uuid.UUID{}, err
	}
	return uuid.FromBytes(uuidBytes)
}
