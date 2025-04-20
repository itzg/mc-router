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

func ReadPacket(reader io.Reader, addr net.Addr, state State) (*Packet, error) {
	logrus.
		WithField("client", addr).
		Debug("Reading packet")

	if state == StateHandshaking {
		bufReader := bufio.NewReader(reader)
		data, err := bufReader.Peek(1)
		if err != nil {
			return nil, err
		}

		if data[0] == PacketIdLegacyServerListPing {
			return ReadLegacyServerListPing(bufReader, addr)
		} else {
			reader = bufReader
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
	if messageName != "MC|PingHost" {
		return nil, errors.Errorf("expected messageName=MC|PingHost, got %s", messageName)
	}

	remainingLen, err := ReadUnsignedShort(reader)
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
		Debug("Reading frame")

	var err error
	frame := &Frame{}

	frame.Length, err = ReadVarInt(reader)
	if err != nil {
		return nil, err
	}

	// Limit frame length to 2^21 - 1
	if frame.Length > 2097151 {
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

func ReadUUID(reader io.Reader) (uuid.UUID, error) {
	buf := make([]byte, 16)
	_, err := reader.Read(buf)
	uuidData, err := uuid.FromBytes(buf)
	if err != nil {
		return uuid.New(), err
	}
	return uuidData, nil
}

func ReadHandshake(data interface{}) (*Handshake, error) {

	dataBytes, ok := data.([]byte)
	if !ok {
		return nil, errors.New("data is not expected byte slice")
	}

	handshake := &Handshake{}
	buffer := bytes.NewBuffer(dataBytes)
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

func ReadLogin(data interface{}) (*Login, error) {
	dataBytes, ok := data.([]byte)
	if !ok {
		return nil, errors.New("data is not expected byte slice")
	}

	login := &Login{}
	buffer := bytes.NewBuffer(dataBytes)
	var err error

	login.Name, err = ReadString(buffer)
	if err != nil {
		return nil, err
	}

	login.PlayerUUID, err = ReadUUID(buffer)
	if err != nil {
		return nil, err
	}

	return login, nil
}
