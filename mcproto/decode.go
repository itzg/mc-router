package mcproto

import (
	"bytes"
	"github.com/pkg/errors"
)

const invalidPacketDataBytesMsg = "data should be byte slice from Packet.Data"

// DecodeHandshake takes the Packet.Data bytes and decodes a Handshake message from it
func DecodeHandshake(data interface{}) (*Handshake, error) {

	dataBytes, ok := data.([]byte)
	if !ok {
		return nil, errors.New(invalidPacketDataBytesMsg)
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
	handshake.NextState = State(nextState)
	return handshake, nil
}

// DecodeLoginStart takes the Packet.Data bytes and decodes a LoginStart message from it
func DecodeLoginStart(data interface{}) (*LoginStart, error) {
	dataBytes, ok := data.([]byte)
	if !ok {
		return nil, errors.New(invalidPacketDataBytesMsg)
	}

	loginStart := &LoginStart{}
	buffer := bytes.NewBuffer(dataBytes)
	var err error

	loginStart.Name, err = ReadString(buffer)
	if err != nil {
		return loginStart, errors.Wrap(err, "failed to read username")
	}

	loginStart.PlayerUuid, err = ReadUuid(buffer)
	if err != nil {
		return loginStart, errors.Wrap(err, "failed to read player uuid")
	}

	return loginStart, nil
}
