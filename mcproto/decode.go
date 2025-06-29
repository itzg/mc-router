package mcproto

import (
	"bytes"
	"strings"

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

	protocolVersion, err := ReadVarInt(buffer)
	if err != nil {
		return nil, err
	}
	handshake.ProtocolVersion = ProtocolVersion(protocolVersion)

	handshake.ServerAddress, err = ReadString(buffer)
	if err != nil {
		return nil, err
	}

	// Forge Mod Loader adds some data after the server address. Truncate it.
	handshake.ServerAddress, _, _ = strings.Cut(handshake.ServerAddress, string(rune(0)))

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
func DecodeLoginStart(protocolVersion ProtocolVersion, data interface{}) (*LoginStart, error) {
	dataBytes, ok := data.([]byte)
	if !ok {
		return nil, errors.New(invalidPacketDataBytesMsg)
	}

	loginStart := NewLoginStart()
	buffer := bytes.NewBuffer(dataBytes)
	var err error

	loginStart.Name, err = ReadString(buffer)
	if err != nil {
		return loginStart, errors.Wrap(err, "failed to read username")
	}

	// These versions can send player keypair data. Ignore it.
	// References:
	// * https://github.com/MCCTeam/Minecraft-Console-Client/blob/f785f509f228bf787c237ac139e6f666a960819a/MinecraftClient/Protocol/Handlers/Protocol18.cs#L2808-L2828
	// * https://minecraft.wiki/w/Minecraft_Wiki:Projects/wiki.vg_merge/Protocol?oldid=2772902#Login_Start
	if protocolVersion >= ProtocolVersion1_19 && protocolVersion <= ProtocolVersion1_19_2 {
		hasSignatureData, err := ReadBoolean(buffer)
		if err != nil {
			return loginStart, errors.Wrap(err, "failed to read has signature data flag")
		}

		if hasSignatureData {
			// Read and discard the data
			_, err = ReadLong(buffer) // Expiration time
			if err != nil {
				return loginStart, errors.Wrap(err, "failed to read expiration time")
			}

			pubKeyLength, err := ReadVarInt(buffer) // Length of the public key
			if err != nil {
				return loginStart, errors.Wrap(err, "failed to read public key length")
			}

			_, err = ReadByteArray(buffer, pubKeyLength) // Public key data
			if err != nil {
				return loginStart, errors.Wrap(err, "failed to read public key")
			}

			signatureLength, err := ReadVarInt(buffer) // Length of the signature
			if err != nil {
				return loginStart, errors.Wrap(err, "failed to read signature length")
			}

			_, err = ReadByteArray(buffer, signatureLength) // Signature data
			if err != nil {
				return loginStart, errors.Wrap(err, "failed to read signature")
			}
		}
	}

	// References:
	// * https://github.com/MCCTeam/Minecraft-Console-Client/blob/f785f509f228bf787c237ac139e6f666a960819a/MinecraftClient/Protocol/Handlers/Protocol18.cs#L2831-L2853
	// * https://minecraft.wiki/w/Minecraft_Wiki:Projects/wiki.vg_merge/Protocol?oldid=2772944#Login_Start
	switch {
	case protocolVersion >= ProtocolVersion1_19_2 && protocolVersion < ProtocolVersion1_20_2:
		// Check to see if a UUID was provided at all
		hasUUID, err := ReadBoolean(buffer)
		if err != nil {
			return loginStart, errors.Wrap(err, "failed to read has uuid flag")
		}

		if !hasUUID {
			break
		}
		fallthrough
	case protocolVersion >= ProtocolVersion1_20_2:
		// For 1.20.2 and later, the UUID is always present
		playerUuid, err := ReadUuid(buffer)
		if err != nil {
			return loginStart, errors.Wrap(err, "failed to read player uuid")
		}
		loginStart.PlayerUuid = playerUuid
	default:
		// For versions before 1.19.2, the UUID is not present
	}

	return loginStart, nil
}
